# Xray IP Tester (CDN IP Checker)

A high-performance, batched CLI tool to test a list of IP addresses against an Xray configuration. It identifies "clean" IPs that successfully tunnel traffic through your specific VLESS/VMess/Shadowsocks setup.

## Features

- **Batched Execution:** Shares a single Xray core instance for multiple IPs (default 20), drastically reducing CPU usage and startup overhead.
- **Concurrent Testing:** Tests all IPs within a batch in parallel.
- **Robust Validation:** Uses strict HTTPS (TLS) 204 status code checks to prevent false positives from random web servers.
- **Clean Output:** Completely silences Xray core logs and initialization warnings for a professional CLI experience.
- **Latency Reporting:** Provides real-time feedback on connection speeds for each IP.

## Prerequisites

1.  **Go:** [Install Go](https://golang.org/doc/install) (1.20+ recommended).
2.  **Xray Assets:** Ensure `geoip.dat` and `geosite.dat` are in the same directory as the executable.
3.  **masscan:** To generate the initial list of IPs.

## Workflow

### 1. Scan for IPs with masscan
Use `masscan` to find IPs with open ports (e.g., 443) and save the output in JSON format.

```bash
sudo masscan -p443 172.64.0.0/16 --rate 10000 -oJ result.json
```

### 2. Prepare Xray Config
Place your working Xray configuration in `config.json`. The tool will automatically use the first inbound as the test listener and the outbound tagged `"proxy"` (or the first outbound) as the test target.

### 3. Build the Tester
```bash
go build -o xray-tester main.go
```

### 4. Run the Tester
```bash
./xray-tester -i result.json -w 20 -t 3 -r 1
```

## CLI Arguments

| Short | Long | Default | Description |
| :--- | :--- | :--- | :--- |
| `-i` | `-input` | `result.json` | Path to the JSON file from masscan |
| `-c` | `-config` | `config.json` | Path to your base Xray config |
| `-o` | `-output` | `clean_ips.txt` | Path to save the working IPs |
| `-w` | `-workers` | `20` | Batch size (IPs per Xray instance) |
| `-t` | `-timeout` | `5` | Connection timeout in seconds |
| `-r` | `-retry` | `0` | Number of retries for failed attempts |

## How it works

The tool works by dynamically rewriting your `config.json` for every batch of IPs. It creates multiple inbounds (different ports) and multiple outbounds (different IPs), then uses strict routing rules to ensure traffic entering port `A` *only* leaves via IP `A`. It then attempts a TLS handshake to `https://www.gstatic.com/generate_204` through each port to verify the tunnel.

## Security Note

This tool hijacks `stderr` and `stdout` at the OS level to suppress Xray's verbose initialization logs. Your normal `fmt.Printf` or `log` calls will still work through a internal `printLog` mechanism.
