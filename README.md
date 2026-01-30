# Xray IP Tester (CDN IP Checker)

A high-performance, concurrent CLI tool to test a list of IP addresses against an Xray configuration. It identifies "clean" IPs that successfully tunnel traffic through your specific VLESS/VMess/Shadowsocks setup.

## Features

- **Worker Pool Architecture:** Uses a fixed number of concurrent workers (default 20) to process IPs continuously from a queue.
- **Isolated Testing:** Each IP check runs in its own isolated Xray Core instance to ensure stability and prevent cross-contamination.
- **Robust Validation:** Uses strict HTTPS (TLS) 204 status code checks to prevent false positives from random web servers.
- **Clean Output:** Completely silences Xray core logs and initialization warnings for a professional CLI experience.
- **Latency Reporting:** Provides real-time feedback on connection speeds for each IP.

## Prerequisites

1.  **Go:** [Install Go](https://golang.org/doc/install) (1.20+ recommended).
2.  **Xray Assets:** Ensure `geoip.dat` and `geosite.dat` are in the same directory as the executable (if your config uses them).
3.  **masscan:** To generate the initial list of IPs (optional, but recommended).

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
go build -o cdnIpChecker main.go
```

### 4. Run the Tester
```bash
./cdnIpChecker -i result.json -w 20 -t 3 -r 1
```

## CLI Arguments

| Short | Long | Default | Description |
| :--- | :--- | :--- | :--- |
| `-i` | `-input` | `result.json` | Path to the JSON file containing IPs |
| `-c` | `-config` | `config.json` | Path to your base Xray config |
| `-o` | `-output` | *(none)* | Path to save the working IPs (optional) |
| `-w` | `-workers` | `20` | Number of concurrent workers |
| `-t` | `-timeout` | `5` | Connection timeout in seconds |
| `-r` | `-retry` | `0` | Number of retries for failed attempts |
| `-h` | `-help` | `false` | Show help message |

## How it works

The tool loads your `config.json` and the list of IPs. It starts a pool of concurrent workers. Each worker picks an IP from the list, creates a temporary Xray configuration specifically for that IP (binding to a unique local port), and attempts to tunnel traffic through it.

It verifies the connection by attempting a TLS handshake to `https://www.gstatic.com/generate_204` (or similar) through the proxy. Strict status code checks ensure that only true proxies (which correctly forward the request and receive the specific 204 response) are marked as working.

## Security Note

This tool hijacks `stderr` and `stdout` at the OS level to suppress Xray's verbose initialization logs. The tool's own output is redirected to the original stdout handle.