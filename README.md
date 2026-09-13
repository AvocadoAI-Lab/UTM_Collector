# Pico-UTM Collector Agent

Go agent that receives UDP Syslog events, stores them in a local spool, and forwards them to an HTTPS collector.

## Requirements

- Go 1.27+
- Linux with systemd, or Windows PowerShell running as Administrator

## Build

```bash
go test ./...
go build -o agent ./cmd/agent
go build -o mockserver ./cmd/mockserver
go build -o syslog-gen ./cmd/syslog-gen
```

Use `./scripts/build.sh` on Linux/macOS or `./scripts/build.ps1` on Windows to create both release folders.

## Quick local test

Build the three binaries, then open three terminals.

Terminal 1 starts the HTTPS mock collector:

```bash
./mockserver -addr 127.0.0.1:18443 -token dev-token -cert-dir ./mock-certs
```

Create `dev-data/config.toml` (use absolute paths if needed):

```toml
[agent]
agent_id = "12345678-1234-1234-1234-123456789abc"
site_id = "local-test"
[listener]
addr = "127.0.0.1"
port = 5514
[forwarder]
endpoint = "https://127.0.0.1:18443"
token = "dev-token"
batch_size = 100
batch_interval_seconds = 5
ca_file = "./mock-certs/mock-ca.pem"
[heartbeat]
interval_seconds = 60
[spool]
dir = "./dev-data/spool"
retention_days = 30
max_size_gb = 1
[logging]
level = "debug"
dir = "./dev-data/logs"
[proxy]
url = ""
```

Terminal 2 starts the agent:

```bash
./agent run --config ./dev-data/config.toml
```

Terminal 3 sends 100 events and checks the result:

```bash
./syslog-gen -target 127.0.0.1:5514 -n 100 -rate 50 -start-id 123456700000000
curl --cacert ./mock-certs/mock-ca.pem https://127.0.0.1:18443/_control/stats
./agent status --config ./dev-data/config.toml
./agent logs --config ./dev-data/config.toml -n 20
```

The mock response should contain `events_accepted: 100`. Stop both processes with `Ctrl+C`; remove local test data with `rm -rf dev-data mock-certs`.

## One-command acceptance test

This installs a temporary service, sends 1000 events, checks status and logs, then deletes test data. Use only in a disposable test VM and type `YES` when prompted.

```bash
sudo ./scripts/verify.sh --count 1000
```

Windows PowerShell (Administrator):

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\scripts\verify.ps1 -Count 1000
```

Use `--keep-running` or `-KeepRunning` to preserve the service, mock, and data after the test.

## Ubuntu 24.04 install

From `dist/pico-utm-agent-linux-amd64`:

```bash
sudo ./install.sh --endpoint https://collector.example.com --token 'TOKEN' --site-id acme-taipei-hq --log-level debug
systemctl status pico-utm-agent --no-pager
systemctl is-enabled pico-utm-agent
sudo ss -ulnp | grep 5514
sudo /usr/local/bin/pico-utm-agent status
sudo /usr/local/bin/pico-utm-agent logs -n 50
```

The service runs as non-root user `pico-utm-agent`; `/etc/pico-utm-agent/config.toml` is mode 600. `--force` reuses the existing `agent_id`.

Uninstall preserves customer configuration, spool, and logs:

```bash
sudo ./uninstall.sh
```

The script prints retained paths and the manual command for complete deletion.

## Windows install

Run PowerShell as Administrator from `dist/pico-utm-agent-windows-amd64`:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\install.ps1 -Endpoint https://collector.example.com -Token 'TOKEN' -SiteId acme-taipei-hq -LogLevel debug
Get-Service pico-utm-agent
& "$env:ProgramFiles\pico-utm-agent\agent.exe" status
& "$env:ProgramFiles\pico-utm-agent\agent.exe" logs -n 50
```
