#!/usr/bin/env bash
# 建置 Windows amd64 與 Linux amd64 發佈包至 dist/
#   ./scripts/build.sh [version]
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=${1:-1.0.0}
LDFLAGS="-s -w -X pico-utm-agent/internal/agent.Version=$VERSION"
export CGO_ENABLED=0 GOARCH=amd64

build() { # goos dir ext scripts...
  local goos=$1 dir=$2 ext=$3; shift 3
  rm -rf "$dir" && mkdir -p "$dir"
  for cmd in agent mockserver syslog-gen; do
    echo "==> $goos/amd64 $cmd"
    GOOS=$goos go build -trimpath -ldflags "$LDFLAGS" -o "$dir/$cmd$ext" "./cmd/$cmd"
  done
  for s in "$@"; do cp "scripts/$s" "$dir/"; done
  cp configs/config.example.toml "$dir/"
}
build windows dist/pico-utm-agent-windows-amd64 .exe install.ps1 uninstall.ps1 verify.ps1
build linux dist/pico-utm-agent-linux-amd64 "" install.sh uninstall.sh verify.sh
chmod +x dist/pico-utm-agent-linux-amd64/{agent,mockserver,syslog-gen,*.sh}
ls -lh dist/*/
