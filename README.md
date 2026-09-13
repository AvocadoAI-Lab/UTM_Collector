# Pico-UTM Collector Agent

Pico-UTM Agent receives UDP Syslog events, stores them in a local spool, and forwards them to an HTTPS collector.

This document is for deployment and operations. The mock server and `verify` scripts in this repository are development tools.

## Requirements

- Ubuntu 24.04 Server with systemd
- root or `sudo` access
- A reachable HTTPS collector URL and valid Bearer token
- UDP port 5514 reachable from the UTM device or sender
- `curl`, `iproute2`, and `ca-certificates` for diagnostics
- A PEM CA file when the collector uses a private CA

Install diagnostic packages if needed:

```bash
sudo apt update
sudo apt install -y curl ca-certificates iproute2
```

## Download the Agent release

Customer deployment hosts should download the Linux Agent package from GitHub Releases; they do not need the repository, Go toolchain, Collector source, or development tools.

```bash
curl -LO https://github.com/AvocadoAI-Lab/UTM_Collector/releases/download/v1.0.0/pico-utm-agent-1.0.0-linux-amd64.tar.gz
curl -LO https://github.com/AvocadoAI-Lab/UTM_Collector/releases/download/v1.0.0/SHA256SUMS-1.0.0.txt
sha256sum -c SHA256SUMS-1.0.0.txt --ignore-missing
tar -xzf pico-utm-agent-1.0.0-linux-amd64.tar.gz
cd pico-utm-agent-1.0.0-linux-amd64
chmod +x agent install.sh uninstall.sh
```

Collector maintainers and developers may clone the complete repository. Build Agent release artifacts with `./scripts/build.sh 1.0.0` or `.\scripts\build.ps1 -Version 1.0.0`; generated files remain under the ignored `dist/` directory.

## Release package

The Agent release package contains only deployment files:

```text
agent                Agent CLI and service process
install.sh           Installer
uninstall.sh         Uninstaller (keeps customer data)
config.example.toml  Configuration reference
README.md            Deployment and troubleshooting instructions
```

Run all commands below from that directory:

```bash
cd /path/to/pico-utm-agent-1.0.0-linux-amd64
chmod +x agent install.sh uninstall.sh
```

## Install on Ubuntu 24.04

Replace the endpoint and token with the values supplied by the collector. The endpoint is the collector base URL; do not append `/events`.

```bash
sudo ./install.sh \
  --endpoint https://collector.example.com \
  --token 'YOUR_BEARER_TOKEN' \
  --site-id acme-taipei-hq \
  --port 5514 \
  --log-level info
```

For a collector using a private CA:

```bash
sudo ./install.sh \
  --endpoint https://collector.example.com \
  --token 'YOUR_BEARER_TOKEN' \
  --site-id acme-taipei-hq \
  --ca-file /absolute/path/collector-ca.pem
```

The installer checks permissions, paths, service name, UDP port, disk space, and existing data before changing anything. It prints every service, firewall, directory, and file operation. Use `--force` for an upgrade; the existing `agent_id` is retained. Use `--regenerate-id` only when the collector requires a new identity.

The service runs as the non-root `pico-utm-agent` account. Configuration is stored at `/etc/pico-utm-agent/config.toml` with mode `600`; spool data is in `/var/lib/pico-utm-agent/spool`; logs are in `/var/log/pico-utm-agent`.

## Verify an installation

```bash
systemctl status pico-utm-agent --no-pager
systemctl is-active pico-utm-agent
systemctl is-enabled pico-utm-agent
sudo ss -ulnp | grep 5514
ps -o user,pid,args -p "$(systemctl show -p MainPID --value pico-utm-agent)"
sudo stat -c '%a %U:%G' /etc/pico-utm-agent/config.toml
sudo /usr/local/bin/pico-utm-agent status
sudo /usr/local/bin/pico-utm-agent logs -n 50
```

Expected results are `active`, `enabled`, a UDP 5514 listener owned by `pico-utm-agent`, and configuration mode `600`.

Optional test from another Linux machine (replace `AGENT_IP`):

```bash
logger --udp --server AGENT_IP --port 5514 \
  '<14>Sep 13 12:00:00 test-host user_act[1]: {"event_id":"manual-test-1","msg":"deployment test"}'
```

After sending, `events_received_total` should increase in `status`; `events_forwarded_total` increases after the collector accepts the batch.

## Reboot check

```bash
sudo reboot
```

After reconnecting:

```bash
systemctl is-active pico-utm-agent
systemctl is-enabled pico-utm-agent
sudo ss -ulnp | grep 5514
sudo /usr/local/bin/pico-utm-agent status
```

## Troubleshooting

View service and application logs:

```bash
sudo journalctl -u pico-utm-agent -n 100 --no-pager
sudo /usr/local/bin/pico-utm-agent logs -n 100
```

If the service is `failed` or repeatedly restarting, inspect the journal for configuration, CA, permission, or collector errors:

```bash
sudo systemctl status pico-utm-agent --no-pager -l
sudo journalctl -u pico-utm-agent -b --no-pager
```

If UDP 5514 is not listening, check the service state and whether another process owns the port:

```bash
sudo ss -ulnp | grep 5514
```

If the collector hostname cannot be resolved:

```bash
getent hosts collector.example.com
```

If TLS validation fails, verify that `--ca-file` points to the correct PEM file and that the service account can read it. If the collector returns 401 or 403, verify the token. Do not put tokens in issue reports or shell history shared with other operators.

If `Spool 積壓` keeps increasing, check `最後轉送錯誤`, collector connectivity, DNS, TLS, token validity, and available disk space:

```bash
sudo /usr/local/bin/pico-utm-agent status
df -h /var/lib/pico-utm-agent
```

If an external sender cannot reach the agent, check the VM or host network mode, routing, and firewall. When UFW is enabled, allow the listener explicitly:

```bash
sudo ufw status
sudo ufw allow 5514/udp
```

## Uninstall

The uninstaller removes the service registration, executable, and firewall rule. It keeps configuration, spool, logs, and the service account to protect customer data:

```bash
sudo ./uninstall.sh
```

It prints every retained path and the manual command for complete deletion. Only run that command after confirming that all retained events and configuration are no longer needed.

After uninstalling:

```bash
systemctl is-active pico-utm-agent || true
systemctl is-enabled pico-utm-agent || true
test ! -e /etc/systemd/system/pico-utm-agent.service && echo PASS-unit-removed
test ! -e /usr/local/bin/pico-utm-agent && echo PASS-binary-removed
sudo ss -ulnp | grep 5514 || echo PASS-port-released
```

## Development tools

`cmd/mockserver`, `scripts/verify.sh`, `scripts/verify.ps1`, and the related tests are for development and CI validation. They are not required on a production deployment host.
