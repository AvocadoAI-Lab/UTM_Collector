#!/usr/bin/env bash
# 卸載 pico-utm-agent：移除 systemd 服務、防火牆規則與執行檔。
# 設定檔、spool、log 與服務帳號會保留（避免誤刪客戶資料），並顯示手動完整清除的方式。
#   sudo ./uninstall.sh
set -uo pipefail

SERVICE=pico-utm-agent
SVC_USER=pico-utm-agent
BIN=/usr/local/bin/pico-utm-agent
CONF_DIR=/etc/pico-utm-agent
CONF=$CONF_DIR/config.toml
DATA_DIR=/var/lib/pico-utm-agent
LOG_DIR=/var/log/pico-utm-agent
UNIT=/etc/systemd/system/$SERVICE.service

FAILED=()
info() { echo "    $*"; }
bad()  { echo "    [✗] $*"; FAILED+=("$*"); }

[[ $EUID -eq 0 ]] || { echo "[✗] 需要 root 權限，請改用 sudo 執行" >&2; exit 1; }

PORT=$(sed -nE 's/^[[:space:]]*port[[:space:]]*=[[:space:]]*([0-9]+).*/\1/p' "$CONF" 2>/dev/null | head -n1)
PORT=${PORT:-5514}

# ---------------------------------------------------------------------------
echo; echo "==> [1/4] 移除 systemd 服務"
if [[ -f $UNIT ]]; then
  info "找到服務：$SERVICE（$(systemctl is-active "$SERVICE" 2>/dev/null)、$(systemctl is-enabled "$SERVICE" 2>/dev/null)）"
  if [[ -x $BIN ]]; then
    if ! "$BIN" uninstall 2>&1 | sed 's/^/    /'; then bad "Service removal failed; executable retained"; exit 1; fi
  else
    info "找不到 $BIN，改用 systemctl 移除"
    systemctl disable --now "$SERVICE" >/dev/null 2>&1 || { bad "Cannot stop service; executable retained"; exit 1; }; info "已停止並取消開機自動啟動"
    rm -f "$UNIT" && info "已刪除 systemd unit：$UNIT"
    systemctl daemon-reload && info "已執行 systemctl daemon-reload"
    systemctl reset-failed "$SERVICE" >/dev/null 2>&1 || true
  fi
  if [[ -f $UNIT ]]; then bad "unit 檔仍存在：$UNIT"; else info "[✓] 服務註冊已移除：$SERVICE"; fi
else
  info "未註冊服務，略過"
fi

# ---------------------------------------------------------------------------
echo; echo "==> [2/4] 移除防火牆規則（UDP $PORT）"
if systemctl is-active --quiet "$SERVICE"; then
  bad "服務仍在執行；保留防火牆規則與執行檔供重試"
  exit 1
fi
PORTS=$( { printf '%s\n' "$PORT"; cat "$CONF_DIR/firewall-ports" 2>/dev/null || true; } | sort -nu)
for PORT in $PORTS; do
  [[ $PORT =~ ^[0-9]+$ ]] && (( PORT >= 1024 && PORT <= 65535 )) || { bad "Invalid retained firewall port"; continue; }
  if command -v ufw >/dev/null 2>&1; then
    # Delete saved rules even when ufw is currently disabled.
    if ufw show added | grep -qE "ufw allow $PORT/udp([[:space:]]|$)"; then
      if ufw --force delete allow "$PORT/udp"; then info "Removed ufw allow $PORT/udp"; else bad "ufw removal failed: $PORT/udp"; fi
    fi
  fi
  if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    if firewall-cmd --permanent --query-port="$PORT/udp" >/dev/null 2>&1; then
      if firewall-cmd --permanent --remove-port="$PORT/udp" && firewall-cmd --reload; then info "Removed firewalld $PORT/udp"; else bad "firewalld removal failed: $PORT/udp"; fi
    fi
  elif command -v firewall-offline-cmd >/dev/null 2>&1; then
    if firewall-offline-cmd --query-port="$PORT/udp" >/dev/null 2>&1; then
      if firewall-offline-cmd --remove-port="$PORT/udp"; then info "Removed offline firewalld $PORT/udp"; else bad "Offline firewalld removal failed: $PORT/udp"; fi
    fi
  fi
done
if (( ${#FAILED[@]} == 0 )); then rm -f "$CONF_DIR/firewall-ports"; fi

# ---------------------------------------------------------------------------
echo; echo "==> [3/4] 移除執行檔"
if [[ -e $BIN || -e $BIN.bak ]]; then
  if rm -f "$BIN" "$BIN.bak"; then info "[✓] 已刪除：$BIN"; else bad "刪除 $BIN 失敗"; fi
else
  info "找不到 $BIN，略過"
fi

# ---------------------------------------------------------------------------
echo; echo "==> [4/4] 保留的資料"
KEPT=0
for p in "$CONF" "$CONF_DIR/status.json" "$DATA_DIR/spool" "$LOG_DIR"; do
  [[ -e $p ]] || continue
  (( KEPT == 0 )) && info "以下資料未刪除（避免誤刪客戶資料）："
  KEPT=1
  info "  $p（$(du -sh "$p" 2>/dev/null | cut -f1)）"
done
if id -u "$SVC_USER" >/dev/null 2>&1; then
  info "  服務帳號 $SVC_USER（上述檔案的擁有者）"
  KEPT=1
fi
if (( KEPT )); then
  AGENT_ID=$(sed -nE 's/^[[:space:]]*agent_id[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' "$CONF" 2>/dev/null | head -n1)
  [[ -n $AGENT_ID ]] && info "重新安裝時會沿用 agent_id：$AGENT_ID"
  info ""
  info "若確定要完整清除（含未轉送的事件），請執行："
  info "  sudo rm -rf $CONF_DIR $DATA_DIR $LOG_DIR && sudo userdel $SVC_USER"
else
  info "沒有保留資料"
fi

echo
if (( ${#FAILED[@]} > 0 )); then
  echo "[✗] 卸載未完全完成（${#FAILED[@]} 項）：" >&2
  for f in "${FAILED[@]}"; do echo "    - $f" >&2; done
  exit 1
fi
echo "[✓] 卸載完成（服務、防火牆規則、執行檔已移除）"
