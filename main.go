package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all"
)

// ScannedIP represents the structure of the objects in the input file
type ScannedIP struct {
	IP        string `json:"ip"`
	Timestamp string `json:"timestamp"`
}

type Result struct {
	IP      string
	Success bool
	Latency time.Duration
	Error   error
}

// Global variable to hold our "real" stdout
var realStdout *os.File

func main() {
	// 1. Hijack Stdout/Stderr before anything else
	stdoutFd, err := syscall.Dup(int(os.Stdout.Fd()))
	if err != nil {
		realStdout = os.Stdout
	} else {
		realStdout = os.NewFile(uintptr(stdoutFd), "/dev/stdout")
		devNull, _ := os.Open(os.DevNull)
		syscall.Dup2(int(devNull.Fd()), 1)
		syscall.Dup2(int(devNull.Fd()), 2)
	}

	// Helper to print to real stdout
	printLog := func(format string, a ...interface{}) {
		fmt.Fprintf(realStdout, format, a...)
	}

	// Define CLI Flags
	inputPath := flag.String("input", "result.json", "Path to the input JSON file")
	flag.StringVar(inputPath, "i", "result.json", "Path to the input JSON file (shorthand)")

	configPath := flag.String("config", "config.json", "Path to the base Xray config.json file")
	flag.StringVar(configPath, "c", "config.json", "Path to the base Xray config.json file (shorthand)")

	outputPath := flag.String("output", "", "Path to save the working IPs (optional)")
	flag.StringVar(outputPath, "o", "", "Path to save the working IPs (shorthand)")

	timeoutSec := flag.Int("timeout", 5, "Timeout in seconds for each proxy test")
	flag.IntVar(timeoutSec, "t", 5, "Timeout in seconds for each proxy test (shorthand)")

	retryCount := flag.Int("retry", 0, "Number of retries for failed connections")
	flag.IntVar(retryCount, "r", 0, "Number of retries for failed connections (shorthand)")

	batchSize := flag.Int("workers", 20, "Batch size (number of IPs to check in a single Xray instance)")
	flag.IntVar(batchSize, "w", 20, "Batch size (shorthand)")

	flag.Parse()

	// 2. Setup asset location
	cwd, err := os.Getwd()
	if err != nil {
		printLog("Error getting current working directory: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("xray.location.asset", cwd)

	// 3. Validate inputs
	if _, err := os.Stat(*inputPath); os.IsNotExist(err) {
		printLog("Error: Input file '%s' not found.\n", *inputPath)
		flag.Usage()
		os.Exit(1)
	}
	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		printLog("Error: Config file '%s' not found.\n", *configPath)
		flag.Usage()
		os.Exit(1)
	}

	// 4. Read Input File
	printLog("Reading IPs from '%s'...\n", *inputPath)
	resultBytes, err := os.ReadFile(*inputPath)
	if err != nil {
		printLog("Error reading input file: %v\n", err)
		os.Exit(1)
	}

	var scannedIPs []ScannedIP
	if err := json.Unmarshal(resultBytes, &scannedIPs); err != nil {
		printLog("Error parsing input JSON: %v\n", err)
		os.Exit(1)
	}
	printLog("Found %d IPs to test.\n", len(scannedIPs))

	// 5. Read Base Config
	printLog("Reading config from '%s'...\n", *configPath)
	configBytes, err := os.ReadFile(*configPath)
	if err != nil {
		printLog("Error reading config file: %v\n", err)
		os.Exit(1)
	}

	// 6. Parse Config Template
	var configTemplate map[string]interface{}
	if err := json.Unmarshal(configBytes, &configTemplate); err != nil {
		printLog("Error parsing config template: %v\n", err)
		os.Exit(1)
	}

	// Find base port
	basePort := 10808
	if inbounds, ok := configTemplate["inbounds"].([]interface{}); ok && len(inbounds) > 0 {
		if first, ok := inbounds[0].(map[string]interface{}); ok {
			if p, ok := first["port"].(float64); ok {
				basePort = int(p)
			}
		}
	} else if inbound, ok := configTemplate["inbound"].(map[string]interface{}); ok {
		if p, ok := inbound["port"].(float64); ok {
			basePort = int(p)
		}
	}

	printLog("Using Base Port: %d. Batch Size: %d. Timeout: %ds. Retries: %d.\n", basePort, *batchSize, *timeoutSec, *retryCount)
	printLog("Starting batched processing (Reports: [Index] IP: Latency/Error)...\n")
	printLog("---------------------------------------------------\n")

	var workingIPs []string
	total := len(scannedIPs)
	processed := 0

	// 7. Process in Batches
	for i := 0; i < total; i += *batchSize {
		end := i + *batchSize
		if end > total {
			end = total
		}
		batchIPs := scannedIPs[i:end]

		results := processBatch(batchIPs, configTemplate, basePort, *timeoutSec, *retryCount)

		for _, res := range results {
			processed++
			if res.Success {
				printLog("[%d/%d] %s: ✅ PASS (%v)\n", processed, total, res.IP, res.Latency)
				workingIPs = append(workingIPs, res.IP)
			} else {
				errMsg := fmt.Sprintf("%v", res.Error)
				if len(errMsg) > 50 {
					errMsg = errMsg[:47] + "..."
				}
				printLog("[%d/%d] %s: ❌ FAIL (%s)\n", processed, total, res.IP, errMsg)
			}
		}
	}

	printLog("---------------------------------------------------\n")
	printLog("Finished! Found %d working IPs.\n", len(workingIPs))

	// 8. Write results
	if *outputPath != "" {
		absOutput, _ := filepath.Abs(*outputPath)
		if len(workingIPs) > 0 {
			f, err := os.Create(*outputPath)
			if err != nil {
				printLog("Error creating output file: %v\n", err)
				os.Exit(1)
			}
			defer f.Close()

			for _, ip := range workingIPs {
				f.WriteString(ip + "\n")
			}
			printLog("Saved working IPs to '%s'\n", absOutput)
		} else {
			printLog("No working IPs found. Nothing to save.\n")
		}
	}
}

func processBatch(ips []ScannedIP, template map[string]interface{}, basePort int, timeoutSec int, retryCount int) []Result {
	// Deep copy template for this batch
	batchConfig := deepCopyMap(template)
	suppressXrayLogging(batchConfig)

	// Prepare structures
	tmplInbound, tmplOutbound := extractTemplates(batchConfig)
	if tmplInbound == nil || tmplOutbound == nil {
		return makeFailResults(ips, fmt.Errorf("invalid config template"))
	}

	newInbounds := []interface{}{}
	// Initialize outbounds with a Blackhole as the first entry (default fallback).
	// This prevents Xray from falling back to out-0 (which might be a working proxy)
	// if a specific outbound fails or routing is ambiguous.
	blackhole := map[string]interface{}{
		"tag":      "fallback-blackhole",
		"protocol": "blackhole",
	}
	newOutbounds := []interface{}{blackhole}

	newRules := []interface{}{}

	testerProtocol := "socks"
	if p, ok := tmplInbound["protocol"].(string); ok {
		testerProtocol = p
	}

	for idx, ipData := range ips {
		port := basePort + idx + 1
		tagSuffix := fmt.Sprintf("-%d", idx)
		inTag := "in" + tagSuffix
		outTag := "out" + tagSuffix

		// 1. Create Inbound
		inbound := deepCopyMap(tmplInbound)
		inbound["port"] = port
		inbound["tag"] = inTag
		inbound["listen"] = "127.0.0.1"
		newInbounds = append(newInbounds, inbound)

		// 2. Create Outbound
		outbound := deepCopyMap(tmplOutbound)
		outbound["tag"] = outTag
		if err := setOutboundAddress(outbound, ipData.IP); err != nil {
			continue
		}
		newOutbounds = append(newOutbounds, outbound)

		// 3. Create Routing Rule
		rule := map[string]interface{}{
			"type":        "field",
			"inboundTag":  []interface{}{inTag},
			"outboundTag": outTag,
		}
		newRules = append(newRules, rule)
	}

	if originalOuts, ok := batchConfig["outbounds"].([]interface{}); ok {
		for _, out := range originalOuts {
			m, _ := out.(map[string]interface{})
			t, _ := m["tag"].(string)
			if t != "proxy" && t != "" {
				newOutbounds = append(newOutbounds, out)
			}
		}
	}

	batchConfig["inbounds"] = newInbounds

	batchConfig["outbounds"] = newOutbounds

	routing, _ := batchConfig["routing"].(map[string]interface{})

	if routing == nil {

		routing = make(map[string]interface{})
		batchConfig["routing"] = routing
	}
	existingRules, _ := routing["rules"].([]interface{})
	routing["rules"] = append(newRules, existingRules...)

	configBytes, _ := json.Marshal(batchConfig)
	coreConfig, err := serial.LoadJSONConfig(bytes.NewReader(configBytes))
	if err != nil {
		return makeFailResults(ips, fmt.Errorf("config build error: %v", err))
	}

	instance, err := core.New(coreConfig)
	if err != nil {
		return makeFailResults(ips, fmt.Errorf("core create error: %v", err))
	}
	if err := instance.Start(); err != nil {
		return makeFailResults(ips, fmt.Errorf("core start error: %v", err))
	}
	defer instance.Close()

	lastPort := basePort + len(ips)
	if !waitForPort(lastPort, 3*time.Second) {
		// Log warning
	}

	var wg sync.WaitGroup
	batchResults := make([]Result, len(ips))

	for i, ipData := range ips {
		wg.Add(1)
		go func(idx int, ipStr string) {
			defer wg.Done()
			port := basePort + idx + 1
			latency, err := testProxy("127.0.0.1", port, testerProtocol, timeoutSec, retryCount)
			batchResults[idx] = Result{IP: ipStr, Success: err == nil, Latency: latency, Error: err}
		}(i, ipData.IP)
	}
	wg.Wait()

	return batchResults
}

func extractTemplates(config map[string]interface{}) (map[string]interface{}, map[string]interface{}) {
	var inbound, outbound map[string]interface{}
	if inbounds, ok := config["inbounds"].([]interface{}); ok && len(inbounds) > 0 {
		inbound, _ = inbounds[0].(map[string]interface{})
	} else if in, ok := config["inbound"].(map[string]interface{}); ok {
		inbound = in
	}
	if outbounds, ok := config["outbounds"].([]interface{}); ok {
		for _, out := range outbounds {
			m, _ := out.(map[string]interface{})
			if tag, ok := m["tag"].(string); ok && tag == "proxy" {
				outbound = m
				break
			}
		}
		if outbound == nil && len(outbounds) > 0 {
			outbound, _ = outbounds[0].(map[string]interface{})
		}
	}
	return inbound, outbound
}

func setOutboundAddress(outbound map[string]interface{}, ip string) error {
	settings, ok := outbound["settings"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("no settings")
	}
	var serverList []interface{}
	if s, ok := settings["servers"].([]interface{}); ok {
		serverList = s
	} else if v, ok := settings["vnext"].([]interface{}); ok {
		serverList = v
	} else {
		return fmt.Errorf("no servers/vnext")
	}
	if len(serverList) == 0 {
		return fmt.Errorf("empty server list")
	}
	server, ok := serverList[0].(map[string]interface{})
	if !ok {
		return fmt.Errorf("invalid server")
	}
	server["address"] = ip
	return nil
}

func suppressXrayLogging(configMap map[string]interface{}) {
	logMap := make(map[string]interface{})
	if existing, ok := configMap["log"].(map[string]interface{}); ok {
		logMap = existing
	}
	logMap["loglevel"] = "none"
	logMap["access"] = ""
	logMap["error"] = ""
	configMap["log"] = logMap
}

func deepCopyMap(m map[string]interface{}) map[string]interface{} {
	var copy map[string]interface{}
	b, _ := json.Marshal(m)
	json.Unmarshal(b, &copy)
	return copy
}

func makeFailResults(ips []ScannedIP, err error) []Result {
	res := make([]Result, len(ips))
	for i, ip := range ips {
		res[i] = Result{IP: ip.IP, Success: false, Error: err}
	}
	return res
}

func waitForPort(port int, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	address := fmt.Sprintf("127.0.0.1:%d", port)
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
			if err == nil {
				conn.Close()
				return true
			}
		}
	}
}

func testProxy(host string, port int, protocol string, timeoutSec int, retryCount int) (time.Duration, error) {
	var proxyURLStr string
	if protocol == "mixed" || protocol == "socks" || protocol == "socks5" {
		proxyURLStr = fmt.Sprintf("socks5://%s:%d", host, port)
	} else {
		proxyURLStr = fmt.Sprintf("http://%s:%d", host, port)
	}

	proxyURL, _ := url.Parse(proxyURLStr)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(proxyURL),
			DisableKeepAlives: true,
		},
		Timeout: time.Duration(timeoutSec) * time.Second,
	}

	// Use HTTPS to ensure the proxy is actually tunneling traffic to the correct destination.
	// A random web server returning 200 OK to a VLESS packet will fail the TLS handshake.
	targetURL := "https://www.gstatic.com/generate_204"

	start := time.Now()
	// Initial attempt + retries
	attempts := 1 + retryCount
	for i := 0; i < attempts; i++ {
		resp, err := client.Get(targetURL)
		if err == nil {
			resp.Body.Close()
			// Strictly require 204.
			if resp.StatusCode == 204 {
				return time.Since(start), nil
			}
			if i == attempts-1 {
				return time.Since(start), fmt.Errorf("status %d", resp.StatusCode)
			}
		}
		// If error, brief pause and retry
		if i < attempts-1 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	return time.Since(start), fmt.Errorf("connection failed after %d attempts", attempts)
}
