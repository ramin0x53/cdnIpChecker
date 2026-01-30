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

	// Ensure flag help output goes to real stdout since we silence stderr
	flag.CommandLine.SetOutput(realStdout)

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

	workerCount := flag.Int("workers", 20, "Number of concurrent workers")
	flag.IntVar(workerCount, "w", 20, "Number of concurrent workers (shorthand)")

	help := flag.Bool("help", false, "Show help message")
	flag.BoolVar(help, "h", false, "Show help message (shorthand)")

	flag.Usage = func() {
		printLog("Usage of cdnIpChecker:\n")
		printLog("  This tool checks a list of IPs against a V2Ray/Xray configuration to find working proxies.\n\n")
		printLog("Flags:\n")
		flag.PrintDefaults()
		printLog("\nExample:\n")
		printLog("  cdnIpChecker -i result.json -c config.json -o working.txt -w 50\n")
	}

	flag.Parse()

	if *help {
		flag.Usage()
		os.Exit(0)
	}

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

	printLog("Using Base Port: %d. Workers: %d. Timeout: %ds. Retries: %d.\n", basePort, *workerCount, *timeoutSec, *retryCount)
	printLog("Starting concurrent processing (Reports: [Index] IP: Latency/Error)...\n")
	printLog("---------------------------------------------------\n")

	// 7. Setup Worker Pool
	jobs := make(chan ScannedIP, len(scannedIPs))
	results := make(chan Result, len(scannedIPs))
	var wg sync.WaitGroup

	// Fill jobs
	for _, ip := range scannedIPs {
		jobs <- ip
	}
	close(jobs)

	// Start workers
	for w := 0; w < *workerCount; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			worker(id, jobs, results, configTemplate, basePort, *timeoutSec, *retryCount)
		}(w)
	}

	// Closer goroutine
	go func() {
		wg.Wait()
		close(results)
	}()

	var workingIPs []string
	processed := 0
	total := len(scannedIPs)

	// Collect results
	for res := range results {
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

func worker(id int, jobs <-chan ScannedIP, results chan<- Result, template map[string]interface{}, basePort, timeout, retry int) {
	for ip := range jobs {
		res := processIP(id, ip, template, basePort, timeout, retry)
		results <- res
	}
}

func processIP(workerID int, ipData ScannedIP, template map[string]interface{}, basePort, timeoutSec, retryCount int) Result {
	// Deep copy template for this check
	ipConfig := deepCopyMap(template)
	suppressXrayLogging(ipConfig)

	// Prepare structures
	tmplInbound, tmplOutbound := extractTemplates(ipConfig)
	if tmplInbound == nil || tmplOutbound == nil {
		return Result{IP: ipData.IP, Success: false, Error: fmt.Errorf("invalid config template")}
	}

	// Calculate unique port for this worker
	port := basePort + workerID + 1

	// 1. Create Inbound
	newInbound := deepCopyMap(tmplInbound)
	newInbound["port"] = port
	newInbound["listen"] = "127.0.0.1"
	newInbound["tag"] = "in-worker"

	// 2. Create Outbound
	newOutbound := deepCopyMap(tmplOutbound)
	if err := setOutboundAddress(newOutbound, ipData.IP); err != nil {
		return Result{IP: ipData.IP, Success: false, Error: err}
	}
	newOutbound["tag"] = "out-worker"

	// Fallback
	blackhole := map[string]interface{}{
		"tag":      "fallback-blackhole",
		"protocol": "blackhole",
	}

	ipConfig["inbounds"] = []interface{}{newInbound}
	ipConfig["outbounds"] = []interface{}{blackhole, newOutbound} // Blackhole first

	// 3. Routing
	routing, _ := ipConfig["routing"].(map[string]interface{})
	if routing == nil {
		routing = make(map[string]interface{})
		ipConfig["routing"] = routing
	}

	rule := map[string]interface{}{
		"type":        "field",
		"inboundTag":  []interface{}{"in-worker"},
		"outboundTag": "out-worker",
	}

	existingRules, _ := routing["rules"].([]interface{})
	routing["rules"] = append([]interface{}{rule}, existingRules...)

	// Build Config
	configBytes, _ := json.Marshal(ipConfig)
	coreConfig, err := serial.LoadJSONConfig(bytes.NewReader(configBytes))
	if err != nil {
		return Result{IP: ipData.IP, Success: false, Error: fmt.Errorf("config build error: %v", err)}
	}

	// Start Xray
	instance, err := core.New(coreConfig)
	if err != nil {
		return Result{IP: ipData.IP, Success: false, Error: fmt.Errorf("core create error: %v", err)}
	}
	if err := instance.Start(); err != nil {
		return Result{IP: ipData.IP, Success: false, Error: fmt.Errorf("core start error: %v", err)}
	}
	// Important: Stop the instance when done to release the port
	defer instance.Close()

	// Wait for port (briefly)
	if !waitForPort(port, 2*time.Second) {
		// Just try anyway, sometimes detection is flaky
	}

	// Test
	testerProtocol := "socks"
	if p, ok := tmplInbound["protocol"].(string); ok {
		testerProtocol = p
	}

	latency, err := testProxy("127.0.0.1", port, testerProtocol, timeoutSec, retryCount)
	return Result{IP: ipData.IP, Success: err == nil, Latency: latency, Error: err}
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
