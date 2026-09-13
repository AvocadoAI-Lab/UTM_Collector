#!/usr/bin/env bash
# 開發測試用一鍵驗收：mock server → 安裝 agent → 灌入事件 → 比對 → 清理。
# 僅限測試環境！會覆蓋安裝 pico-utm-agent，結束後刪除其設定檔、spool、log 與服務帳號。
#   sudo ./verify.sh [--count N] [--timeout 秒] [--keep-running]
set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
COUNT=1000
TIMEOUT=180
KEEP=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --count) COUNT=${2:?--count 需要數字}; shift 2 ;;
    --timeout) TIMEOUT=${2:?--timeout 需要秒數}; shift 2 ;;
    --keep-running) KEEP=1; shift ;;
    -h|--help) echo "用法：sudo $0 [--count N] [--timeout 秒] [--keep-running]"; exit 0 ;;
    *) echo "未知參數：$1" >&2; exit 2 ;;
  esac
done

[[ $COUNT =~ ^[1-9][0-9]*$ && $TIMEOUT =~ ^[1-9][0-9]*$ ]] || { echo "Count and timeout must be positive integers" >&2; exit 2; }
SERVICE=pico-utm-agent
SVC_USER=pico-utm-agent
BIN=/usr/local/bin/pico-utm-agent
CONF_DIR=/etc/pico-utm-agent
CONF=$CONF_DIR/config.toml
STATUS=$CONF_DIR/status.json
DATA_DIR=/var/lib/pico-utm-agent
LOG_DIR=/var/log/pico-utm-agent
MOCK_PORT=18443
UDP_PORT=5514
ENDPOINT="https://127.0.0.1:$MOCK_PORT"
WORK=/var/lib/pico-utm-agent-verify
CA=$WORK/certs/mock-ca.pem
TOKEN="verify-$(tr -d '-' </proc/sys/kernel/random/uuid)"

section() { echo; echo "==> $*"; }
info() { echo "    $*"; }

# ---- 警告與確認 ------------------------------------------------------------
cat <<EOF
==================================================================
 警告：此腳本僅供開發測試環境使用，請勿在客戶環境執行！
==================================================================
 將會：
  1. 在 127.0.0.1:$MOCK_PORT 啟動 mock server
  2. 以 --force 覆蓋安裝 $SERVICE 服務，指向 mock
  3. 送出 $COUNT 筆測試事件到 UDP $UDP_PORT 並比對結果
EOF
if (( KEEP )); then
  echo "  4. --keep-running：結束後保留服務、mock server 與所有資料"
else
  echo "  4. 結束後移除服務，並【刪除】以下資料："
  echo "       $CONF_DIR $DATA_DIR $LOG_DIR（設定檔、spool、log）"
  echo "       $WORK 與服務帳號 $SVC_USER"
fi
echo

[[ $EUID -eq 0 ]] || { echo "[✗] 需要 root 權限，請改用 sudo 執行" >&2; exit 1; }
for f in agent mockserver syslog-gen install.sh uninstall.sh; do
  [[ -f $DIR/$f ]] || { echo "[✗] 找不到 $DIR/$f（請在建置產物資料夾 pico-utm-agent-linux-amd64 中執行）" >&2; exit 1; }
done
command -v curl >/dev/null 2>&1 || { echo "[✗] 找不到 curl（sudo apt install curl）" >&2; exit 1; }
for tool in python3 ss systemctl; do
  command -v "$tool" >/dev/null || { echo "Missing prerequisite: $tool" >&2; exit 1; }
done
for p in "$CONF_DIR" "$DATA_DIR" "$LOG_DIR" "$WORK" "$BIN"; do
  [[ ! -e $p && ! -L $p ]] || { echo "Existing data/install path: $p; refusing to overwrite. Clean up manually first." >&2; exit 1; }
done
if systemctl list-unit-files "$SERVICE.service" --no-legend | grep -q "$SERVICE.service"; then
  echo "Service already exists; refusing to overwrite" >&2; exit 1
fi
USER_EXISTED=0
id -u "$SVC_USER" >/dev/null 2>&1 && USER_EXISTED=1
echo "Any failure preserves the service, mock and data for debugging."
read -r -p "確認要繼續？請輸入 YES：" ANSWER
[[ $ANSWER == "YES" ]] || { echo "已取消，未做任何變更。"; exit 1; }
chmod +x "$DIR/agent" "$DIR/mockserver" "$DIR/syslog-gen" "$DIR"/*.sh

# ---- 輔助函式 --------------------------------------------------------------
RESULTS=()
MOCK_PID=""
record() { # 名稱 通過(1/0) [說明]
  local st=FAIL
  [[ $2 == 1 ]] && st=PASS
  RESULTS+=("$st|$1|${3:-}")
  echo "    [$st] $1${3:+ — $3}"
}
bool() { if "$@"; then echo 1; else echo 0; fi; }
mock_get() { curl -s --max-time 5 --cacert "$CA" -H "Authorization: Bearer $TOKEN" "$ENDPOINT$1" 2>/dev/null; }
json_num() { grep -oE "\"$1\": *-?[0-9]+" | head -n1 | grep -oE -- '-?[0-9]+$'; }
json_str() { grep -oE "\"$1\": *(\"[^\"]*\"|null)" | head -n1 | sed -E 's/^"[^"]*": *//; s/^"//; s/"$//'; }

run_verify() {
  # ---- 1 -------------------------------------------------------------------
  section "1. 啟動 mock server"
  mkdir -p "$WORK/certs" || { record "1. create mock directory" 0 "$WORK"; return; }
  chmod 755 "$WORK" "$WORK/certs" # 服務帳號需能讀取 CA 憑證
  if ss -H -tln "sport = :$MOCK_PORT" 2>/dev/null | grep -q .; then
    record "1. mock server 啟動" 0 "TCP $MOCK_PORT 已被佔用"
    return
  fi
  "$DIR/mockserver" -addr "127.0.0.1:$MOCK_PORT" -token "$TOKEN" -cert-dir "$WORK/certs" >"$WORK/mock.log" 2>&1 &
  MOCK_PID=$!
  local up=0
  for _ in $(seq 60); do
    sleep 0.5
    kill -0 "$MOCK_PID" 2>/dev/null || break
    if mock_get /healthz | grep -q '"ok":true'; then up=1; break; fi
  done
  record "1. mock server 啟動（$ENDPOINT）" $up "PID $MOCK_PID，log：$WORK/mock.log"
  (( up )) || return

  # ---- 2 -------------------------------------------------------------------
  section "2. 安裝並啟動 agent（指向 mock）"
  bash "$DIR/install.sh" --endpoint "$ENDPOINT" --token "$TOKEN" --site-id verify-test \
    --ca-file "$CA" --log-level debug --force
  local rc=$?
  local installed
  installed=$(bool eval '[[ $rc -eq 0 ]] && systemctl is-active --quiet $SERVICE')
  record "2. agent 安裝並啟動（systemd active）" "$installed" "install.sh exit $rc"
  (( installed )) || return

  # ---- 3 -------------------------------------------------------------------
  section "3. syslog-gen 送出 $COUNT 筆"
  "$DIR/syslog-gen" -target "127.0.0.1:$UDP_PORT" -n "$COUNT" -rate 500 -start-id 123456700000000 2>&1 | sed 's/^/    /'
  rc=${PIPESTATUS[0]}
  record "3. syslog-gen 送出 $COUNT 筆" "$(bool test "$rc" -eq 0)" "exit $rc"
  (( rc == 0 )) || return

  # ---- 4 -------------------------------------------------------------------
  section "4. 等待轉送完成（逾時 ${TIMEOUT} 秒）"
  local start=$SECONDS accepted=0
  while (( SECONDS - start < TIMEOUT )); do
    accepted=$(mock_get /_control/stats | json_num events_accepted)
    accepted=${accepted:-0}
    (( accepted >= COUNT )) && break
    sleep 2
  done
  record "4. 轉送完成" "$(bool test "$accepted" -ge "$COUNT")" "接收端 accepted=$accepted，耗時 $((SECONDS - start)) 秒"

  # ---- 5 -------------------------------------------------------------------
  section "5. 比對接收端 unique 事件數"
  sleep 3
  local stats; stats=$(mock_get /_control/stats)
  local acc rcv dup batches hbs
  acc=$(json_num events_accepted <<<"$stats"); rcv=$(json_num events_received <<<"$stats")
  dup=$(json_num events_duplicated <<<"$stats"); batches=$(json_num batches_received <<<"$stats"); hbs=$(json_num heartbeats <<<"$stats")
  record "5. unique 事件數 = $COUNT" "$(bool test "${acc:--1}" -eq "$COUNT")" \
    "accepted=${acc:-?}，received=${rcv:-?}，duplicated=${dup:-?}，batches=${batches:-?}，heartbeats=${hbs:-?}"

  # ---- 6 -------------------------------------------------------------------
  section "6. 檢查 status.json"
  local st="" fwd
  start=$SECONDS
  while (( SECONDS - start < 45 )); do
    st=$(cat "$STATUS" 2>/dev/null)
    fwd=$(json_num events_forwarded_total <<<"$st")
    (( ${fwd:-0} >= COUNT )) && break
    sleep 2
  done
  if [[ -z $st ]]; then
    record "6. status.json 可讀取" 0 "$STATUS"
  else
    python3 -c 'import json,sys; d=json.load(sys.stdin); keys="updated_at events_received_total events_forwarded_total events_dead_lettered_total events_dropped_total spool_write_errors_total spool_backlog_bytes last_event_received_at last_forward_success_at last_heartbeat_at last_forward_error agent_id utm_source_ips proxy_in_use config".split(); assert all(k in d for k in keys)' <<<"$st"
    record "6. status.json valid JSON and required fields" "$(bool test $? -eq 0)"
    local updated age conf_id st_id got
    updated=$(json_str updated_at <<<"$st")
    age=$(( $(date +%s) - $(date -d "$updated" +%s 2>/dev/null || echo 0) ))
    record "6. status.json：updated_at 在 60 秒內" "$(bool test "$age" -ge -5 -a "$age" -lt 60)" "updated_at=$updated（$age 秒前）"
    for pair in "events_received_total:$COUNT" "events_forwarded_total:$COUNT" "events_dead_lettered_total:0" \
                "events_dropped_total:0" "spool_write_errors_total:0" "spool_backlog_bytes:0"; do
      got=$(json_num "${pair%%:*}" <<<"$st")
      record "6. status.json：${pair%%:*} = ${pair##*:}" "$(bool test "${got:--1}" -eq "${pair##*:}")" "實際 ${got:-無}"
    done
    for k in last_event_received_at last_forward_success_at last_heartbeat_at; do
      got=$(json_str "$k" <<<"$st")
      record "6. status.json：$k 非 null" "$(bool test -n "$got" -a "$got" != null)" "$got"
    done
    got=$(json_str last_forward_error <<<"$st")
    record "6. status.json：last_forward_error 為 null" "$(bool test "$got" = null)" "$got"
    conf_id=$(sed -nE 's/^[[:space:]]*agent_id[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' "$CONF" | head -n1)
    st_id=$(json_str agent_id <<<"$st")
    record "6. status.json：agent_id 與設定檔一致" "$(bool test -n "$st_id" -a "$st_id" = "$conf_id")" "$st_id"
    record "6. status.json：utm_source_ips 含 127.0.0.1" "$(bool grep -q '"127.0.0.1"' <<<"$st")"
    record "6. status.json：proxy_in_use = false" "$(bool grep -qE '"proxy_in_use": *false' <<<"$st")"
    record "6. status.json：config.log_level = debug" "$(bool grep -qE '"log_level": *"debug"' <<<"$st")"
    record "6. status.json：不含 token" "$(bool eval '! grep -qF "$TOKEN" <<<"$st"')"
  fi

  # ---- 7 -------------------------------------------------------------------
  section "7. 檢查 log 與執行帳號"
  local logs; logs=$(ls "$LOG_DIR"/agent-*.log 2>/dev/null | tr '\n' ' ')
  record "7. log 檔存在" "$(bool test -n "$logs")" "$logs"
  if [[ -n $logs ]] && ! grep -rqF "$TOKEN" "$LOG_DIR"; then
    record "7. log 不含 token" 1
  else
    record "7. log 不含 token" 0 "$(grep -rlF "$TOKEN" "$LOG_DIR" 2>/dev/null | tr '\n' ' ')"
  fi
  local batch_lines; batch_lines=$(grep -rhF '批次轉送成功' "$LOG_DIR" 2>/dev/null | wc -l)
  record "7. log 含批次轉送紀錄" "$(bool test "$batch_lines" -gt 0)" "$batch_lines 筆"
  "$BIN" logs -n 5 >/dev/null 2>&1
  record "7. agent logs 子命令可執行" "$(bool test $? -eq 0)"
  local pid user
  pid=$(systemctl show -p MainPID --value "$SERVICE")
  user=$(ps -o user= -p "$pid" 2>/dev/null | tr -d ' ')
  record "7. 服務執行帳號非 root" "$(bool test -n "$user" -a "$user" != root)" "user=${user:-?}"
  record "7. 設定檔權限為 600" "$(bool test "$(stat -c %a "$CONF")" = 600)" "$(stat -c '%a %U:%G' "$CONF")"
}

cleanup() {
  for result in "${RESULTS[@]}"; do [[ $result != FAIL* ]] || KEEP=1; done
  if (( KEEP )); then
    section "保留現場（--keep-running）"
    info "服務狀態：systemctl status $SERVICE"
    [[ -n $MOCK_PID ]] && kill -0 "$MOCK_PID" 2>/dev/null && info "mock server：PID $MOCK_PID，$ENDPOINT，log $WORK/mock.log"
    info "mock 統計：curl -s --cacert $CA $ENDPOINT/_control/stats"
    info "agent 狀態：sudo $BIN status"
    info "agent log ：sudo $BIN logs -n 100"
    info "手動清理："
    info "  sudo $DIR/uninstall.sh"
    [[ -n $MOCK_PID ]] && info "  kill $MOCK_PID"
    info "  sudo rm -rf $CONF_DIR $DATA_DIR $LOG_DIR $WORK && sudo userdel $SVC_USER"
    return
  fi
  section "清理"
  if ! bash "$DIR/uninstall.sh"; then
    record "9. uninstall cleanup" 0 "Data and mock preserved; uninstall failed"
    return
  fi
  record "9. uninstall cleanup" 1
  if [[ -n $MOCK_PID ]] && kill -0 "$MOCK_PID" 2>/dev/null; then
    kill "$MOCK_PID"; wait "$MOCK_PID" 2>/dev/null
    info "已關閉 mock server（PID $MOCK_PID）"
  fi
  for d in "$CONF_DIR" "$DATA_DIR" "$LOG_DIR" "$WORK"; do
    [[ -e $d ]] || continue
    if [[ -L $d ]]; then record "9. cleanup path" 0 "Refusing symlink $d"; continue; fi
    if rm -rf -- "$d"; then record "9. removed test data $d" 1; else record "9. removed test data $d" 0; fi
  done
  if (( USER_EXISTED == 0 )) && id -u "$SVC_USER" >/dev/null 2>&1; then
    if userdel "$SVC_USER"; then record "9. removed test service account" 1; else record "9. removed test service account" 0; fi
  fi
}

trap 'record "Interrupted" 0; KEEP=1; cleanup; exit 130' INT TERM
run_verify
trap - INT TERM
cleanup

# ---- 結果 ------------------------------------------------------------------
section "驗收結果"
FAILS=()
for r in "${RESULTS[@]}"; do
  IFS='|' read -r st name detail <<<"$r"
  echo "    [$st] $name"
  [[ $st == PASS ]] || FAILS+=("$name：$detail")
done
echo
if (( ${#RESULTS[@]} > 0 && ${#FAILS[@]} == 0 )); then
  echo "PASS：${#RESULTS[@]} 項全部通過"
  exit 0
fi
echo "FAIL：${#FAILS[@]} / ${#RESULTS[@]} 項未通過"
for f in "${FAILS[@]}"; do echo "    - $f"; done
(( KEEP )) || echo "    提示：加上 --keep-running 可保留現場除錯"
exit 1
