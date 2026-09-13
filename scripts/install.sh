#!/usr/bin/env bash
# 安裝 pico-utm-agent 為 systemd 服務。
#   sudo ./install.sh --endpoint https://collector.example.com --token xxx --site-id acme-taipei-hq
set -Eeuo pipefail

SERVICE=pico-utm-agent
SVC_USER=pico-utm-agent
BIN=/usr/local/bin/pico-utm-agent
CONF_DIR=/etc/pico-utm-agent
CONF=$CONF_DIR/config.toml
DATA_DIR=/var/lib/pico-utm-agent
SPOOL_DIR=$DATA_DIR/spool
LOG_DIR=/var/log/pico-utm-agent
UNIT=/etc/systemd/system/$SERVICE.service
SRC_DIR="$(cd "$(dirname "$0")" && pwd)"
SRC=$SRC_DIR/agent
TOTAL=12
BACKUP=""

ENDPOINT="" TOKEN="" SITE_ID="" PORT=5514 LOG_LEVEL=info PROXY_URL="" CA_FILE="" FORCE=0 REGEN=0
STEP=0
STEP_NAME=""
PROBLEMS=()
RB_DESC=()
RB_CMD=()

step() { STEP=$((STEP + 1)); STEP_NAME=$1; echo; echo "==> [$STEP/$TOTAL] $1"; }
info() { echo "    $*"; }
ok()   { echo "    [✓] $*"; }
bad()  { echo "    [✗] $*"; PROBLEMS+=("$*"); }
die()  { echo "[✗] $*" >&2; exit 1; }
add_rollback() { RB_DESC+=("$1"); RB_CMD+=("$2"); }

usage() {
  cat <<EOF
用法：sudo $0 --endpoint URL --token TOKEN --site-id ID [選項]

  --endpoint URL      接收端根網址（必須 https://）
  --token TOKEN       Bearer token
  --site-id ID        站點代碼（^[a-z0-9][a-z0-9-]{2,63}$）
  --port N            監聽 UDP 埠（預設 5514）
  --log-level LEVEL   debug / info / warn / error（預設 info）
  --proxy-url URL     HTTP proxy（預設不使用）
  --ca-file PATH      額外信任的 CA 憑證（PEM）
  --force             覆蓋既有安裝（沿用既有 agent_id）
  --regenerate-id     產生新的 agent_id
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --force) FORCE=1; shift; continue ;;
    --regenerate-id) REGEN=1; shift; continue ;;
    -h|--help) usage; exit 0 ;;
  esac
  [[ $# -ge 2 ]] || die "$1 需要參數值（執行 $0 --help 查看用法）"
  case "$1" in
    --endpoint) ENDPOINT=$2 ;;
    --token) TOKEN=$2 ;;
    --site-id) SITE_ID=$2 ;;
    --port) PORT=$2 ;;
    --log-level) LOG_LEVEL=$2 ;;
    --proxy-url) PROXY_URL=$2 ;;
    --ca-file) CA_FILE=$2 ;;
    *) die "未知參數：$1（執行 $0 --help 查看用法）" ;;
  esac
  shift 2
done

toml_str() { local s=${1//\\/\\\\}; s=${s//\"/\\\"}; printf '"%s"' "$s"; }
ufw_active() { command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; }
firewalld_active() { command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; }

# ---------------------------------------------------------------------------
step "檢查權限"
[[ $EUID -eq 0 ]] || die "需要 root 權限，請改用 sudo 執行"
ok "以 root 執行"
command -v systemctl >/dev/null 2>&1 || die "找不到 systemctl（需要 systemd）"
ok "systemd 可用"

# ---------------------------------------------------------------------------
step "安裝前預檢（尚未做任何變更）"
[[ $ENDPOINT =~ ^https://[^/[:space:]]+ ]] && ok "endpoint：$ENDPOINT" || bad "--endpoint 必須是 https:// 開頭的合法 URL（目前為 '$ENDPOINT'）"
[[ -n ${TOKEN//[[:space:]]/} ]] && ok "token：已提供（不顯示）" || bad "--token 不可為空"
[[ $SITE_ID =~ ^[a-z0-9][a-z0-9-]{2,63}$ ]] && ok "site_id：$SITE_ID" || bad "--site-id 必須符合 ^[a-z0-9][a-z0-9-]{2,63}$（目前為 '$SITE_ID'）"
if [[ $PORT =~ ^[0-9]+$ ]] && (( PORT >= 1024 && PORT <= 65535 )); then ok "監聽埠：UDP $PORT"; else bad "--port 必須介於 1024–65535（目前為 '$PORT'）"; fi
[[ $LOG_LEVEL =~ ^(debug|info|warn|error)$ ]] && ok "log_level：$LOG_LEVEL" || bad "--log-level 必須為 debug / info / warn / error（目前為 '$LOG_LEVEL'）"
if [[ -z $PROXY_URL ]]; then ok "proxy：未設定（仍會套用 HTTPS_PROXY / NO_PROXY 環境變數）"
elif [[ $PROXY_URL =~ ^https?:// ]]; then ok "proxy：已設定"
else bad "--proxy-url 必須是 http:// 或 https:// 開頭"; fi
if [[ -n $CA_FILE ]]; then
  if [[ -f $CA_FILE ]]; then
    CA_FILE="$(cd "$(dirname "$CA_FILE")" && pwd)/$(basename "$CA_FILE")"
    ok "ca_file：$CA_FILE（服務帳號 $SVC_USER 必須可讀取此檔與其上層目錄）"
  else bad "--ca-file 找不到檔案：$CA_FILE"; fi
fi
[[ -f $SRC ]] && ok "安裝來源：$SRC" || bad "找不到 $SRC（install.sh 須與 agent 放在同一資料夾）"

EXISTING=0
if [[ -f $UNIT ]] || systemctl list-unit-files "$SERVICE.service" --no-legend 2>/dev/null | grep -q "$SERVICE.service"; then
  EXISTING=1
  SVC_STATE=$(systemctl is-active "$SERVICE" 2>/dev/null || true)
  if [[ $FORCE -eq 1 ]]; then ok "服務名稱 $SERVICE 已註冊（$SVC_STATE）；--force：將停止後更新並沿用註冊"
  else bad "服務名稱 $SERVICE 已被註冊（$SVC_STATE）。覆蓋安裝請加 --force；移除請執行 uninstall.sh"; fi
else
  ok "服務名稱 $SERVICE 未被佔用"
fi

if [[ -e $BIN ]]; then
  if [[ $FORCE -eq 1 ]]; then ok "安裝路徑 $BIN 已有檔案；--force：將覆蓋"
  else bad "安裝路徑 $BIN 已有檔案。覆蓋安裝請加 --force；移除請執行 uninstall.sh"; fi
else
  ok "安裝路徑 $BIN 可用"
fi
[[ -f $CONF ]] && info "發現既有設定檔 $CONF：將沿用其 agent_id（舊檔備份為 $CONF.bak）"

if command -v ss >/dev/null 2>&1; then
  PORT_USERS=$(ss -H -ulnp "sport = :$PORT" 2>/dev/null || true)
  if [[ -z $PORT_USERS ]]; then
    ok "UDP $PORT 未被佔用"
  else
    MAIN_PID=$(systemctl show -p MainPID --value "$SERVICE" 2>/dev/null || echo 0)
    OWNERS=$(grep -o 'users:.*' <<<"$PORT_USERS" | tr '\n' ' ')
    if [[ $EXISTING -eq 1 && $FORCE -eq 1 && ${MAIN_PID:-0} != 0 && $(grep -oE 'pid=[0-9]+' <<<"$PORT_USERS" | sort -u) == "pid=$MAIN_PID" ]]; then
      ok "UDP $PORT 由既有的 agent 服務使用（PID $MAIN_PID），升級時會先停止"
    else
      bad "UDP $PORT 已被佔用：${OWNERS:-$PORT_USERS}"
    fi
  fi
else
  info "找不到 ss，略過埠佔用檢查"
fi

AVAIL_KB=$(df -Pk /var/lib | awk 'NR==2 {print $4}')
if (( AVAIL_KB >= 1048576 )); then ok "磁碟（/var/lib）可用 $((AVAIL_KB / 1024)) MB"
else bad "磁碟（/var/lib）可用空間不足 1GB（$((AVAIL_KB / 1024)) MB）"; fi

for tool in ss runuser install stat df awk sed useradd; do
  command -v "$tool" >/dev/null 2>&1 || bad "Required command missing: $tool"
done
for p in "$BIN" "$CONF" "$UNIT" "$CONF_DIR" "$DATA_DIR" "$SPOOL_DIR" "$LOG_DIR"; do
  [[ ! -L $p ]] || bad "Refusing symbolic link: $p"
done
for d in "$CONF_DIR" "$DATA_DIR" "$SPOOL_DIR" "$LOG_DIR"; do
  [[ ! -e $d || -d $d ]] || bad "Not a directory: $d"
done
for p in "$BIN" "$CONF" "$UNIT"; do
  [[ ! -e $p || -f $p ]] || bad "Not a regular file: $p"
done
for p in /usr/local/bin /etc /var/lib /var/log; do
  [[ -w $p ]] || bad "Path is not writable: $p"
  free=$(df -Pk "$p" | awk 'NR==2 {print $4}')
  (( free >= 1048576 )) || bad "Less than 1 GiB available on $p"
done
if [[ -f $CONF && $REGEN -ne 1 ]]; then
  ids=$(sed -nE 's/^[[:space:]]*agent_id[[:space:]]*=[[:space:]]*"([0-9a-fA-F-]{36})".*/\1/p' "$CONF")
  [[ $ids =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]] || bad "Existing agent_id is invalid; refusing to replace it"
fi
if (( EXISTING )); then
  if [[ ! -f $UNIT ]] || ! grep -Fxq "ExecStart=$BIN run --config $CONF" "$UNIT" || ! grep -Fxq "User=$SVC_USER" "$UNIT"; then
    bad "Service name is occupied by another unit; --force cannot take it over"
  fi
fi
for value in "$ENDPOINT" "$TOKEN" "$SITE_ID" "$PROXY_URL" "$CA_FILE"; do
  [[ ! $value =~ [[:cntrl:]] ]] || bad "Configuration values cannot contain control characters"
done

if (( ${#PROBLEMS[@]} > 0 )); then
  echo
  echo "[✗] 預檢未通過（${#PROBLEMS[@]} 項），未做任何變更。" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
on_error() {
  local rc=$?
  trap - ERR INT TERM
  set +e
  local rollback_failed=0
  echo
  echo "[✗] 安裝失敗於步驟 [$STEP/$TOTAL] $STEP_NAME（exit $rc）" >&2
  if (( ${#RB_CMD[@]} > 0 )); then
    echo "==> 回復已完成的變更"
    for ((i = ${#RB_CMD[@]} - 1; i >= 0; i--)); do
      if eval "${RB_CMD[$i]}" >/dev/null 2>&1; then info "已回復：${RB_DESC[$i]}"
      else echo "    [!] 回復失敗：${RB_DESC[$i]}"; rollback_failed=1; fi
    done
  fi
  if [[ -n $BACKUP ]]; then
    if (( rollback_failed == 0 )); then rm -rf -- "$BACKUP"; else info "Rollback backup retained: $BACKUP"; fi
  fi
  exit "$rc"
}
trap on_error ERR
trap 'false' INT TERM

# ---------------------------------------------------------------------------
step "停止既有服務（保留原始註冊）"
if [[ $EXISTING -eq 1 ]]; then
  WAS_ACTIVE=$(systemctl is-active "$SERVICE" 2>/dev/null || true)
  WAS_ENABLED=$(systemctl is-enabled "$SERVICE" 2>/dev/null || true)
  restore_service() {
    systemctl daemon-reload
    if [[ $WAS_ENABLED == enabled ]]; then systemctl enable "$SERVICE"; else systemctl disable "$SERVICE"; fi
    if [[ $WAS_ACTIVE == active ]]; then systemctl start "$SERVICE"; fi
  }
  add_rollback "Restore original service state $SERVICE" restore_service
  systemctl stop "$SERVICE"
  info "Stopped $SERVICE; original unit and service settings retained"
else
  info "No existing service"
fi
BACKUP=$(mktemp -d /var/tmp/pico-utm-agent-install.XXXXXXXX)
chmod 700 "$BACKUP"
info "Rollback snapshot: $BACKUP"
snapshot() {
  local path=$1 key=$2
  if [[ -e $path ]]; then
    cp -a "$path" "$BACKUP/$key"
    add_rollback "Restore $path" "cp -a '$BACKUP/$key' '$path'"
  else
    add_rollback "Remove new file $path" "rm -f '$path'"
  fi
}

# ---------------------------------------------------------------------------
step "建立服務帳號"
if id -u "$SVC_USER" >/dev/null 2>&1; then
  info "帳號已存在，沿用：$SVC_USER（uid $(id -u "$SVC_USER")）"
else
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$SVC_USER"
  info "建立系統帳號：$SVC_USER（uid $(id -u "$SVC_USER")，無登入 shell）"
  add_rollback "刪除帳號 $SVC_USER" "userdel '$SVC_USER'"
fi

# ---------------------------------------------------------------------------
step "複製執行檔"
snapshot "$BIN" agent
install -m 0755 "$SRC" "$BIN"
info "複製：$SRC → $BIN（0755）"

# ---------------------------------------------------------------------------
step "決定 agent_id"
AGENT_ID=""
if [[ -f $CONF && $REGEN -ne 1 ]]; then
  AGENT_ID=$(sed -nE 's/^[[:space:]]*agent_id[[:space:]]*=[[:space:]]*"([0-9a-fA-F-]{36})".*/\1/p' "$CONF" | head -n1)
fi
if [[ -n $AGENT_ID ]]; then
  info "沿用既有 agent_id：$AGENT_ID（來自 $CONF）"
else
  AGENT_ID=$(cat /proc/sys/kernel/random/uuid)
  if [[ $REGEN -eq 1 ]]; then info "--regenerate-id：產生新的 agent_id：$AGENT_ID"; else info "產生新的 agent_id：$AGENT_ID"; fi
fi

# ---------------------------------------------------------------------------
step "寫入設定檔並限制權限"
if [[ ! -d $CONF_DIR ]]; then
  add_rollback "Remove new directory $CONF_DIR" "rmdir '$CONF_DIR'"
  install -d -m 0700 -o "$SVC_USER" -g "$SVC_USER" "$CONF_DIR"
else
  OLD_CONF_MODE=$(stat -c %a "$CONF_DIR")
  OLD_CONF_OWNER=$(stat -c %u:%g "$CONF_DIR")
  add_rollback "Restore directory permissions $CONF_DIR" "chmod '$OLD_CONF_MODE' '$CONF_DIR'; chown '$OLD_CONF_OWNER' '$CONF_DIR'"
  chmod 700 "$CONF_DIR"
  chown "$SVC_USER:$SVC_USER" "$CONF_DIR"
fi
snapshot "$CONF" config.toml
snapshot "$CONF_DIR/status.json" status.json
snapshot "$CONF_DIR/firewall-ports" firewall-ports
TMP_CONF=$BACKUP/new-config.toml
cat >"$TMP_CONF" <<EOF
[agent]
agent_id = "$AGENT_ID"  # 安裝時產生，勿手動修改
site_id  = $(toml_str "$SITE_ID")

[listener]
addr = "0.0.0.0"
port = $PORT

[forwarder]
endpoint = $(toml_str "$ENDPOINT")
token = $(toml_str "$TOKEN")
batch_size = 500
batch_interval_seconds = 60
ca_file = $(toml_str "$CA_FILE")

[heartbeat]
interval_seconds = 300

[spool]
dir = "$SPOOL_DIR"
retention_days = 30
max_size_gb = 10

[logging]
level = "$LOG_LEVEL"
dir = "$LOG_DIR"

[proxy]
url = $(toml_str "$PROXY_URL")
EOF
install -m 0600 -o "$SVC_USER" -g "$SVC_USER" "$TMP_CONF" "$CONF"
rm -f "$TMP_CONF"
info "寫入：$CONF（site_id=$SITE_ID、port=$PORT、log_level=$LOG_LEVEL）"
info "權限：$CONF_DIR 0700、$CONF 0600，擁有者 $SVC_USER"

# ---------------------------------------------------------------------------
step "建立 spool 與 log 目錄"
for d in "$DATA_DIR" "$SPOOL_DIR" "$LOG_DIR"; do
  if [[ -d $d ]]; then
    mode=$(stat -c %a "$d"); owner=$(stat -c %u:%g "$d")
    add_rollback "Restore permissions $d" "chmod '$mode' '$d'; chown '$owner' '$d'"
  else
    add_rollback "Remove new directory $d (only if empty)" "rm -rf -- '$d'"
  fi
  install -d -m 0750 -o "$SVC_USER" -g "$SVC_USER" "$d"
  info "Directory $d: 0750 $SVC_USER:$SVC_USER"
done


# ---------------------------------------------------------------------------
step "設定防火牆"
if ufw_active || firewalld_active; then
  { cat "$CONF_DIR/firewall-ports" 2>/dev/null || true; printf '%s\n' "$PORT"; } | sort -nu >"$BACKUP/firewall-ports.new"
  install -m 0600 "$BACKUP/firewall-ports.new" "$CONF_DIR/firewall-ports"
fi
if ufw_active; then
  if ufw status | grep -qE "^$PORT/udp[[:space:]]+ALLOW"; then
    info "ufw 已有規則 $PORT/udp ALLOW，沿用"
  else
    add_rollback "Remove ufw UDP $PORT" "ufw --force delete allow '$PORT/udp'"
    ufw allow "$PORT/udp" comment pico-utm-agent >/dev/null
    info "新增 ufw 規則：allow $PORT/udp"
  fi
elif firewalld_active; then
  if firewall-cmd --permanent --query-port="$PORT/udp" >/dev/null 2>&1; then
    info "firewalld 已開放 $PORT/udp，沿用"
  else
    add_rollback "Remove firewalld UDP $PORT" "firewall-cmd --permanent --remove-port='$PORT/udp' && firewall-cmd --reload"
    firewall-cmd --permanent --add-port="$PORT/udp" >/dev/null
    firewall-cmd --reload >/dev/null
    info "新增 firewalld 規則：--permanent --add-port=$PORT/udp"
  fi
else
  info "未偵測到啟用中的 ufw / firewalld，略過（若使用其他防火牆，請自行開放 UDP $PORT）"
fi

# ---------------------------------------------------------------------------
step "註冊 systemd 服務"
if [[ $EXISTING -eq 0 ]]; then
  add_rollback "Remove service $SERVICE" "if [[ -f '$UNIT' ]]; then '$BIN' uninstall; fi"
  "$BIN" install --config "$CONF" --user "$SVC_USER" 2>&1 | sed 's/^/    /'
else
  systemctl enable "$SERVICE"
  info "Reused service registration: $UNIT"
fi
add_rollback "Stop agent before restoring files" "systemctl stop '$SERVICE'"

# ---------------------------------------------------------------------------
step "執行 agent test"
runuser -u "$SVC_USER" -- "$BIN" test --config "$CONF" 2>&1 | sed 's/^/    /'

# ---------------------------------------------------------------------------
step "啟動服務"
systemctl start "$SERVICE"
sleep 3
if ! systemctl is-active --quiet "$SERVICE"; then
  journalctl -u "$SERVICE" -n 20 --no-pager 2>/dev/null | sed 's/^/    /' || true
  false # 觸發回復
fi
MAIN_PID=$(systemctl show -p MainPID --value "$SERVICE")
info "服務狀態：active（PID $MAIN_PID，執行帳號 $(ps -o user= -p "$MAIN_PID" | tr -d ' ')）"

trap - ERR INT TERM
rm -rf -- "$BACKUP"
echo
echo "[✓] 安裝完成"
info "服務      ：$SERVICE（active、enabled）"
info "agent_id  ：$AGENT_ID"
info "執行檔    ：$BIN"
info "設定檔    ：$CONF"
info "spool     ：$SPOOL_DIR"
info "log       ：$LOG_DIR"
echo
info "查看狀態：sudo pico-utm-agent status"
info "查看 log ：sudo pico-utm-agent logs -n 50"
info "即時事件：sudo pico-utm-agent tail"
info "卸載    ：sudo $SRC_DIR/uninstall.sh"
