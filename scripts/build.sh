#!/usr/bin/env bash
# 建置 Windows amd64 與 Linux amd64 Agent 發佈包至 dist/
#   ./scripts/build.sh [version]
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=${1:-1.0.0}
LDFLAGS="-s -w -X pico-utm-agent/internal/agent.Version=$VERSION"
export CGO_ENABLED=0 GOARCH=amd64

build() { # goos dir ext scripts...
  local goos=$1 dir=$2 ext=$3; shift 3
  rm -rf "$dir" && mkdir -p "$dir"
  echo "==> $goos/amd64 agent"
  GOOS=$goos go build -trimpath -ldflags "$LDFLAGS" -o "$dir/agent$ext" ./cmd/agent
  for s in "$@"; do cp "scripts/$s" "$dir/"; done
  cp configs/config.example.toml "$dir/"
  cp README.md "$dir/"
}
WINDOWS_DIR="dist/pico-utm-agent-$VERSION-windows-amd64"
LINUX_DIR="dist/pico-utm-agent-$VERSION-linux-amd64"
build windows "$WINDOWS_DIR" .exe install.ps1 uninstall.ps1
build linux "$LINUX_DIR" "" install.sh uninstall.sh
chmod +x "$LINUX_DIR"/{agent,*.sh}
tar -czf "$LINUX_DIR.tar.gz" -C dist "$(basename "$LINUX_DIR")"
(
  cd dist
  if command -v zip >/dev/null 2>&1; then
    zip -qr "$(basename "$WINDOWS_DIR").zip" "$(basename "$WINDOWS_DIR")"
  else
    echo "zip 未安裝，略過 Windows zip；可直接使用目錄 $WINDOWS_DIR" >&2
  fi
  sha256sum "$(basename "$LINUX_DIR").tar.gz" "$(basename "$WINDOWS_DIR").zip" 2>/dev/null >"SHA256SUMS-$VERSION.txt" ||
    sha256sum "$(basename "$LINUX_DIR").tar.gz" >"SHA256SUMS-$VERSION.txt"
)
ls -lh dist/*.{tar.gz,zip,txt} 2>/dev/null || true
