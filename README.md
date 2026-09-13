# Pico-UTM Collector

This repository contains both sides of the Pico-UTM event pipeline:

- **Agent** runs inside the customer network, receives UDP Syslog events, stores them in a local spool, and forwards them over HTTPS.
- **Collector API Server** runs on an Internet-reachable server and persists events and Agent heartbeats in PostgreSQL.

```text
Pico-UTM --UDP 5514--> Agent --HTTPS 443--> Collector API --> PostgreSQL
```

The mock server, `syslog-gen`, and `verify` scripts are development tools and are not required in production.

## Agent requirements

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

## Deploy the Collector API Server

The Collector deployment runs Caddy, the Go API, and PostgreSQL with Docker Compose. Caddy obtains a public TLS certificate and exposes HTTPS. PostgreSQL and the API container are not published directly to the Internet.

### Server requirements

- An Internet-reachable Linux server with a persistent disk
- A domain such as `collector.example.com` with an A/AAAA record pointing to the server
- Inbound TCP ports 80 and 443 allowed by the cloud firewall, router, and host firewall
- Docker Engine with the Compose plugin
- Git for obtaining and updating the repository

Verify the domain before starting Caddy:

```bash
getent hosts collector.example.com
```

The returned address must match the server's public IP. Port 80 is required for initial certificate issuance and renewal; Agents connect to port 443.

### Configure and start

Clone the repository on the Collector server:

```bash
git clone https://github.com/AvocadoAI-Lab/UTM_Collector.git
cd UTM_Collector/deploy/collector
cp env.example .env
```

Generate separate random values for the database password and Agent token:

```bash
openssl rand -hex 32
openssl rand -hex 32
```

Edit `.env` and replace the example domain and secrets:

```dotenv
COLLECTOR_DOMAIN=collector.example.com
POSTGRES_DB=collector
POSTGRES_USER=collector
POSTGRES_PASSWORD=REPLACE_WITH_DATABASE_PASSWORD
COLLECTOR_TOKENS=REPLACE_WITH_AGENT_TOKEN
MIN_SUPPORTED_VERSION=1.0.0
MAX_BODY_BYTES=67108864
```

Protect the file and start the stack:

```bash
chmod 600 .env
docker compose up -d --build
docker compose ps
```

All three services should be running; PostgreSQL should become `healthy`. The Collector automatically applies its idempotent schema migration at startup. Database data and Caddy TLS state remain in named Docker volumes.

### Verify the Collector

Load the first configured token without printing it, then call the authenticated health endpoint:

```bash
set -a
. ./.env
set +a
TOKEN=${COLLECTOR_TOKENS%%,*}
curl --fail --silent --show-error \
  -H "Authorization: Bearer $TOKEN" \
  "https://${COLLECTOR_DOMAIN}/healthz"
unset TOKEN COLLECTOR_TOKENS POSTGRES_PASSWORD
```

Expected response:

```json
{"ok":true,"server_time":"2026-09-13T12:00:00.000Z"}
```

If verification fails, inspect the services and logs:

```bash
docker compose ps
docker compose logs --tail=100 collector
docker compose logs --tail=100 caddy
docker compose logs --tail=100 postgres
```

The production API exposes only these authenticated endpoints:

```text
GET  /healthz
POST /events
POST /heartbeat
```

It accepts gzip and uncompressed JSON, limits compressed and decompressed bodies to 64 MiB by default, writes event batches transactionally, and deduplicates retried batches and events. A successful `/healthz` also confirms PostgreSQL connectivity.

### Connect an Agent

Use the Collector domain and one of the tokens from `COLLECTOR_TOKENS` when installing the Agent. Pass the base URL without `/events`:

```bash
sudo ./install.sh \
  --endpoint https://collector.example.com \
  --token 'THE_SAME_AGENT_TOKEN' \
  --site-id acme-taipei-hq \
  --port 5514 \
  --log-level info
```

Caddy uses a public certificate, so `--ca-file` is unnecessary. After sending a Syslog event, verify that `收到事件` and `已轉送` increase:

```bash
sudo /usr/local/bin/pico-utm-agent status
sudo /usr/local/bin/pico-utm-agent logs -n 50
```

On the Collector server, a successful request appears in the Caddy access log without a storage error in the Collector log:

```bash
docker compose logs --since=10m caddy collector
```

### Token rotation

`COLLECTOR_TOKENS` accepts multiple comma-separated tokens. Add the new token alongside the old token, recreate the Collector, update the Agents, and then remove the old token:

```dotenv
COLLECTOR_TOKENS=NEW_TOKEN,OLD_TOKEN
```

```bash
docker compose up -d --no-deps --force-recreate collector
```

Tokens are compared as SHA-256 digests in constant time and are not written to application logs. Never commit `.env` or paste its contents into issue reports.

### Backup and update

Create a PostgreSQL backup outside the Docker volume:

```bash
set -a
. ./.env
set +a
docker compose exec -T postgres \
  pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc > "collector-$(date +%F).dump"
unset POSTGRES_PASSWORD COLLECTOR_TOKENS
```

Update the source and recreate changed containers:

```bash
cd /path/to/UTM_Collector
git pull --ff-only
cd deploy/collector
docker compose up -d --build
docker compose ps
```

Do not run `docker compose down -v` unless the PostgreSQL data and Caddy state should be deleted.

### Direct development

Direct, non-container development requires a reachable PostgreSQL database:

```bash
export DATABASE_URL='postgres://collector:password@localhost:5432/collector?sslmode=disable'
export COLLECTOR_TOKENS='development-token'
go run ./cmd/collector
```

Run the test suite with:

```bash
go test ./...
```
