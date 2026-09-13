package agent

// 整合測試：對應 Agent 規格書第 11.2 節各情境。
// agent 以真實 UDP 與 HTTPS 連到 in-process 的 mock server；時間經由 FakeClock 推進。

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pico-utm-agent/internal/mockserver"
)

const testToken = "integration-test-token"

func dpiEvent(id int) []byte {
	return []byte(fmt.Sprintf(`<14>Sep 11 16:32:46 Pico-UTM-A8C24600C6B2-MyPico dpi_event[2554]: {"action":"Block","domain":"malware.wicar.org","event_id":"%015d","module":"ips","severity":"Critical","ts":"1789115566"}`, id))
}

type harness struct {
	t             *testing.T
	cfg           *Config
	clock         *FakeClock
	mock          *mockserver.Server
	ts            *httptest.Server
	logs          *syncBuffer
	maxSpoolBytes int64

	agent  *Agent
	cancel context.CancelFunc
	done   chan error
}

func newHarness(t *testing.T, start time.Time) *harness {
	dir := t.TempDir()
	mock := mockserver.New(testToken)
	ts := httptest.NewTLSServer(mock)
	t.Cleanup(ts.Close)
	cfg := &Config{
		Agent:     AgentConfig{AgentID: "550e8400-e29b-41d4-a716-446655440000", SiteID: "acme-taipei-hq"},
		Listener:  ListenerConfig{Addr: "127.0.0.1", Port: 0},
		Forwarder: ForwarderConfig{Endpoint: ts.URL, Token: testToken, BatchSize: 10, BatchIntervalSeconds: 60},
		Heartbeat: HeartbeatConfig{IntervalSeconds: 3600},
		Spool:     SpoolConfig{Dir: filepath.Join(dir, "spool"), RetentionDays: 30, MaxSizeGB: 10},
		Logging:   LoggingConfig{Level: "debug", Dir: filepath.Join(dir, "logs")},
		Path:      filepath.Join(dir, "config.toml"),
	}
	return &harness{t: t, cfg: cfg, clock: NewFakeClock(start), mock: mock, ts: ts, logs: &syncBuffer{}}
}

func (h *harness) start() {
	h.t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(h.ts.Certificate())
	logger := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a, err := New(h.cfg, Options{Clock: h.clock, Logger: logger, RootCAs: pool, MaxSpoolBytes: h.maxSpoolBytes})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.agent, h.cancel, h.done = a, cancel, make(chan error, 1)
	go func() { h.done <- a.Run(ctx) }()
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		h.t.Error("agent 未在 10 秒內停止")
	}
}

func (h *harness) send(from, to int) {
	h.t.Helper()
	conn, err := net.DialUDP("udp", nil, h.agent.UDPAddr())
	if err != nil {
		h.t.Fatal(err)
	}
	defer conn.Close()
	for id := from; id <= to; id++ {
		if _, err := conn.Write(dpiEvent(id)); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) stats() statsValues { return h.agent.stats.snapshot() }

// waitUntil 輪詢條件；advance > 0 時每輪推進假時鐘，讓批次間隔與退避等待到期。
func (h *harness) waitUntil(desc string, advance time.Duration, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			logs := h.logs.String()
			if len(logs) > 4000 {
				logs = "…" + logs[len(logs)-4000:]
			}
			h.t.Fatalf("逾時等待：%s\nmock 統計：%+v\nlog：\n%s", desc, h.mock.Stats(), logs)
		}
		if advance > 0 {
			h.clock.Advance(advance)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) accepted() int { return h.mock.Stats().EventsAccepted }

func (h *harness) assertAccepted(ranges ...[2]int) {
	h.t.Helper()
	var want []string
	for _, r := range ranges {
		for id := r[0]; id <= r[1]; id++ {
			want = append(want, fmt.Sprintf("%015d", id))
		}
	}
	got := h.mock.Accepted()
	if len(got) != len(want) {
		h.t.Fatalf("接收端收到 %d 筆（去重後），預期 %d 筆", len(got), len(want))
	}
	for i := range want {
		if got[i].EventID != want[i] {
			h.t.Fatalf("第 %d 筆 event_id = %s，預期 %s（順序錯誤或遺失）", i, got[i].EventID, want[i])
		}
	}
}

func (h *harness) spoolPath(name string) string { return filepath.Join(h.cfg.Spool.Dir, name) }

func (h *harness) assertCommittedAtEnd() {
	h.t.Helper()
	c := h.agent.spool.Committed()
	st, err := os.Stat(h.spoolPath(c.File))
	if err != nil || c.Offset != st.Size() {
		h.t.Fatalf("offset %+v 應指向 spool 檔尾（%v）", c, err)
	}
	data, _ := os.ReadFile(h.spoolPath(offsetFileName))
	var onDisk Position
	if json.Unmarshal(data, &onDisk) != nil || onDisk != c {
		h.t.Fatalf("offset 檔內容 %s 與記憶體 %+v 不一致", data, c)
	}
}

var day = time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)

// 正常收送：事件完整送達，順序正確
func TestIntegrationNormalDelivery(t *testing.T) {
	h := newHarness(t, day)
	h.start()
	h.send(1, 25)
	h.waitUntil("receiver 收到 25 筆", 0, func() bool { return h.stats().received == 25 })
	h.waitUntil("前 20 筆依 batch_size 送達", 0, func() bool { return h.accepted() == 20 })
	if h.mock.Stats().BatchesReceived != 2 {
		t.Fatalf("未達 batch_interval 前應只送出 2 個滿載批次，實際 %d", h.mock.Stats().BatchesReceived)
	}
	h.waitUntil("剩餘 5 筆依 batch_interval 送達", 5*time.Second, func() bool { return h.accepted() == 25 })
	h.assertAccepted([2]int{1, 25})
	h.assertCommittedAtEnd()
}

// 端點回 500：持續重試，offset 不動，恢復後續傳
func TestIntegrationServer500(t *testing.T) {
	h := newHarness(t, day)
	h.mock.SetFault(mockserver.Fault{Status: 500})
	h.start()
	h.send(1, 10)
	h.waitUntil("收到至少 4 次 500", 2*time.Second, func() bool { return h.mock.Stats().StatusCounts["500"] >= 4 })
	if c := h.agent.spool.Committed(); c != (Position{}) {
		t.Fatalf("500 時 offset 不得推進：%+v", c)
	}
	if _, err := os.Stat(h.spoolPath(offsetFileName)); !os.IsNotExist(err) {
		t.Fatal("500 時不應寫入 offset 檔")
	}
	h.mock.ClearFault()
	h.waitUntil("恢復後送達", 5*time.Second, func() bool { return h.accepted() == 10 })
	h.assertAccepted([2]int{1, 10})
	h.assertCommittedAtEnd()
}

// 端點回 400：進 dead-letter，offset 推進，不卡死
func TestIntegrationServer400DeadLetter(t *testing.T) {
	h := newHarness(t, day)
	h.cfg.Forwarder.BatchSize = 5
	h.mock.SetFault(mockserver.Fault{Status: 400, Count: 1})
	h.start()
	h.send(1, 5)
	h.waitUntil("批次進入 dead-letter", 0, func() bool { return h.stats().deadLettered == 5 })
	h.assertCommittedAtEnd()

	data, err := os.ReadFile(h.spoolPath("dead-letter-2026-09-11.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var dl deadLetter
	if err := json.Unmarshal(data, &dl); err != nil || dl.Status != 400 || len(dl.Batch.Events) != 5 || !strings.Contains(dl.ResponseBody, "injected_fault") {
		t.Fatalf("dead-letter 內容不正確：%v %s", err, data)
	}

	h.send(6, 10)
	h.waitUntil("後續批次正常送達", 0, func() bool { return h.accepted() == 5 })
	h.assertAccepted([2]int{6, 10})
	if n := h.mock.Stats().StatusCounts["400"]; n != 1 {
		t.Fatalf("400 不應重試，實際收到 %d 次", n)
	}
}

// 端點回 429：依 Retry-After 退避
func TestIntegrationServer429RetryAfter(t *testing.T) {
	h := newHarness(t, day)
	h.cfg.Forwarder.BatchSize = 5
	h.mock.SetFault(mockserver.Fault{Status: 429, RetryAfter: "7", Count: 1})
	h.start()
	h.send(1, 5)
	h.waitUntil("forwarder 依 Retry-After 等待 7 秒", 0, func() bool { return h.clock.HasWaiter(7 * time.Second) })

	h.clock.Advance(6 * time.Second)
	time.Sleep(300 * time.Millisecond)
	if n := h.mock.Stats().PathCounts["/events"]; n != 1 {
		t.Fatalf("未滿 Retry-After 不應重送，實際 /events 請求 %d 次", n)
	}
	h.clock.Advance(1 * time.Second)
	h.waitUntil("滿 7 秒後重送成功", 0, func() bool { return h.accepted() == 5 })
	h.assertAccepted([2]int{1, 5})
}

// 網路中斷 10 分鐘：恢復後所有事件送達，無遺失
func TestIntegrationNetworkOutage10Min(t *testing.T) {
	h := newHarness(t, day)
	h.mock.SetFault(mockserver.Fault{Drop: true})
	h.start()
	for m := 0; m < 10; m++ { // 每分鐘進 10 筆，持續 10 分鐘
		h.send(m*10+1, m*10+10)
		h.waitUntil("receiver 收到事件", 0, func() bool { return h.stats().received == int64((m+1)*10) })
		for i := 0; i < 12; i++ {
			h.clock.Advance(5 * time.Second)
			time.Sleep(5 * time.Millisecond)
		}
	}
	st := h.mock.Stats()
	if st.EventsAccepted != 0 || st.Drops < 6 {
		t.Fatalf("中斷期間應持續重試（≥6 次）且無事件送達：%+v", st)
	}
	if c := h.agent.spool.Committed(); c != (Position{}) {
		t.Fatalf("中斷期間 offset 不得推進：%+v", c)
	}
	h.mock.ClearFault()
	h.waitUntil("恢復後全部送達", 5*time.Second, func() bool { return h.accepted() == 100 })
	h.assertAccepted([2]int{1, 100})
	h.assertCommittedAtEnd()
	t.Logf("中斷期間斷線重試 %d 次", st.Drops)
}

// 轉送中強制終止進程：重啟後從 offset 續傳，可接受重複
func TestIntegrationKillDuringForward(t *testing.T) {
	h := newHarness(t, day)
	h.mock.SetFault(mockserver.Fault{DelayMS: 1500, Count: 1})
	h.start()
	h.send(1, 10)
	h.waitUntil("批次已送出、等待回應中", 0, func() bool { return h.mock.Stats().PathCounts["/events"] >= 1 })
	time.Sleep(200 * time.Millisecond)
	h.stop() // 在收到回應前終止
	if c := h.agent.spool.Committed(); c != (Position{}) {
		t.Fatalf("未收到回應不得推進 offset：%+v", c)
	}
	time.Sleep(1500 * time.Millisecond) // 接收端仍會處理完那筆請求，重啟後的重送即為重複

	h.start()
	h.waitUntil("重啟後重送", 0, func() bool { return h.mock.Stats().BatchesReceived >= 2 })
	h.waitUntil("去重後 10 筆完整", 0, func() bool { return h.accepted() == 10 })
	h.assertAccepted([2]int{1, 10})
	h.assertCommittedAtEnd()
	t.Logf("接收端統計：%+v", h.mock.Stats())
}

// spool 達 max_size：刪除最舊檔案，記錄 WARN，服務不中斷
func TestIntegrationSpoolMaxSize(t *testing.T) {
	h := newHarness(t, day)
	h.cfg.Forwarder.BatchSize = 5
	os.MkdirAll(h.cfg.Spool.Dir, 0o750)
	writeLines := func(name string, from, to int) {
		for id := from; id <= to; id++ {
			appendFile(t, h.spoolPath(name), spoolLine(id))
		}
	}
	writeLines("spool-2026-09-08.jsonl", 1, 20)
	writeLines("spool-2026-09-09.jsonl", 21, 40)
	h.mock.SetFault(mockserver.Fault{Status: 503})
	h.start()
	h.waitUntil("積壓批次送出中（503）", 0, func() bool { return h.mock.Stats().StatusCounts["503"] >= 1 })

	// 啟動後才調降上限，避免啟動時的清理先於第一個批次讀取
	st2, _ := os.Stat(h.spoolPath("spool-2026-09-09.jsonl"))
	h.agent.janitorMu.Lock()
	h.agent.maxSpoolBytes = st2.Size() + 10
	h.agent.janitorMu.Unlock()
	h.agent.runJanitor()
	if _, err := os.Stat(h.spoolPath("spool-2026-09-08.jsonl")); !os.IsNotExist(err) {
		t.Fatal("應刪除最舊的 spool 檔")
	}
	// 最舊檔 20 筆中，前 5 筆正在轉送（仍會送達），其餘 15 筆視為丟棄
	if d := h.stats().dropped; d != 15 {
		t.Fatalf("events_dropped_total = %d，預期 15", d)
	}
	if hb := h.agent.heartbeatPayload(); hb.EventsDroppedTotal != 15 {
		t.Fatalf("心跳 events_dropped_total = %d", hb.EventsDroppedTotal)
	}
	if !strings.Contains(h.logs.String(), `"level":"WARN","msg":"刪除 spool 檔"`) {
		t.Fatal("刪除 spool 檔應記錄 WARN")
	}

	h.mock.ClearFault()
	h.send(41, 45)
	h.waitUntil("服務不中斷，後續事件送達", 5*time.Second, func() bool { return h.accepted() == 30 })
	h.assertAccepted([2]int{1, 5}, [2]int{21, 45})
}

// 磁碟寫滿：記錄 ERROR，不 crash
func TestIntegrationDiskFull(t *testing.T) {
	h := newHarness(t, day)
	h.start()
	h.agent.spool.setWriteHook(func() error { return errors.New("磁碟空間不足（模擬）") })
	h.send(1, 5)
	h.waitUntil("寫入失敗計數", 0, func() bool { return h.stats().spoolWriteErrors == 5 })
	if !strings.Contains(h.logs.String(), `"level":"ERROR"`) {
		t.Fatal("磁碟寫滿應記錄 ERROR")
	}
	select {
	case <-h.done:
		t.Fatal("agent 不應結束")
	default:
	}
	h.agent.spool.setWriteHook(nil)
	h.send(6, 15)
	h.waitUntil("空間恢復後正常送達", 5*time.Second, func() bool { return h.accepted() == 10 })
	h.assertAccepted([2]int{6, 15})
}

// 高流量：1000 筆/秒，無封包遺失（以計數比對）
func TestIntegrationHighTraffic(t *testing.T) { runHighTraffic(t, 5*time.Second) }

func TestIntegrationHighTraffic60s(t *testing.T) {
	if os.Getenv("PICO_LONG_TEST") == "" {
		t.Skip("設定 PICO_LONG_TEST=1 以執行 60 秒高流量測試")
	}
	runHighTraffic(t, 60*time.Second)
}

func runHighTraffic(t *testing.T, dur time.Duration) {
	h := newHarness(t, day)
	h.cfg.Forwarder.BatchSize = 500
	h.start()
	conn, err := net.DialUDP("udp", nil, h.agent.UDPAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	total := int(dur.Seconds()) * 1000
	begin := time.Now()
	for id := 1; id <= total; {
		target := min(int(time.Since(begin).Seconds()*1000)+1, total)
		for ; id <= target; id++ {
			conn.Write(dpiEvent(id))
		}
		time.Sleep(time.Millisecond)
	}
	sendTime := time.Since(begin)

	h.waitUntil("receiver 收到全部事件", 0, func() bool { return h.stats().received == int64(total) })
	h.waitUntil("全部送達接收端", 5*time.Second, func() bool { return h.accepted() == total })
	h.assertAccepted([2]int{1, total})

	var spoolLines int64
	for _, f := range listSpoolFiles(h.cfg.Spool.Dir) {
		n, _ := countLines(h.spoolPath(f.name), 0)
		spoolLines += n
	}
	if spoolLines != int64(total) {
		t.Fatalf("spool 行數 %d，預期 %d", spoolLines, total)
	}
	t.Logf("送出 %d 筆，耗時 %s（%.0f 筆/秒）；receiver %d 筆、spool %d 行、接收端 %d 筆、批次 %d 個",
		total, sendTime.Round(time.Millisecond), float64(total)/sendTime.Seconds(), h.stats().received, spoolLines, h.accepted(), h.mock.Stats().BatchesReceived)
}

// 跨日：spool 正確切檔，offset 正確跨檔
func TestIntegrationDayRollover(t *testing.T) {
	h := newHarness(t, time.Date(2026, 9, 11, 23, 59, 30, 0, time.UTC))
	h.cfg.Forwarder.BatchSize = 100
	h.start()
	h.send(1, 5)
	h.waitUntil("收到跨日前事件", 0, func() bool { return h.stats().received == 5 })
	h.clock.Advance(40 * time.Second) // 跨過 UTC 午夜
	h.send(6, 10)
	h.waitUntil("收到跨日後事件", 0, func() bool { return h.stats().received == 10 })

	for _, name := range []string{"spool-2026-09-11.jsonl", "spool-2026-09-12.jsonl"} {
		if n, err := countLines(h.spoolPath(name), 0); err != nil || n != 5 {
			t.Fatalf("%s 應有 5 行，實際 %d（%v）", name, n, err)
		}
	}
	h.waitUntil("跨檔批次送達", 5*time.Second, func() bool { return h.accepted() == 10 })
	h.assertAccepted([2]int{1, 10})
	if c := h.agent.spool.Committed(); c.File != "spool-2026-09-12.jsonl" {
		t.Fatalf("offset 應跨到新檔：%+v", c)
	}
	h.assertCommittedAtEnd()

	h.agent.runJanitor()
	if _, err := os.Stat(h.spoolPath("spool-2026-09-11.jsonl.gz")); err != nil {
		t.Fatal("轉送完成的前一日檔案應被 gzip")
	}
	if _, err := os.Stat(h.spoolPath("spool-2026-09-11.jsonl")); !os.IsNotExist(err) {
		t.Fatal("壓縮後應刪除原檔")
	}
}

// 心跳：欄位完整、失敗不影響事件轉送、版本過舊僅提醒
func TestIntegrationHeartbeat(t *testing.T) {
	h := newHarness(t, day)
	h.cfg.Forwarder.BatchSize = 5
	h.start()
	h.waitUntil("啟動時送出心跳", 0, func() bool { return h.mock.Stats().Heartbeats >= 1 })
	var hb map[string]any
	json.Unmarshal(h.mock.Stats().LastHeartbeat, &hb)
	for _, k := range []string{"agent_id", "site_id", "agent_version", "hostname", "os", "sent_at", "uptime_seconds",
		"last_event_received_at", "events_received_total", "events_forwarded_total", "events_dead_lettered_total",
		"events_dropped_total", "spool_backlog_bytes", "spool_total_bytes", "spool_disk_free_bytes",
		"last_forward_success_at", "last_forward_error", "proxy_in_use", "utm_source_ips"} {
		if _, ok := hb[k]; !ok {
			t.Errorf("心跳缺少欄位 %s", k)
		}
	}

	// 心跳端點故障不影響事件轉送
	h.mock.SetFault(mockserver.Fault{Status: 500, Paths: []string{"/heartbeat"}})
	h.agent.sendHeartbeat(context.Background())
	h.send(1, 5)
	h.waitUntil("心跳失敗時事件照常送達", 0, func() bool { return h.accepted() == 5 })
	h.mock.ClearFault()

	h.mock.SetMinSupportedVersion("9.0.0")
	h.agent.sendHeartbeat(context.Background())
	if !strings.Contains(h.logs.String(), "min_supported_version") {
		t.Fatal("版本低於 min_supported_version 應記錄 WARN")
	}
	h.agent.writeStatus()
	st, err := readStatus(h.cfg.Path)
	if err != nil || !st.UpgradeRequired || st.EventsForwardedTotal != 5 || len(st.UTMSourceIPs) != 1 {
		t.Fatalf("status.json 內容不正確：%v %+v", err, st)
	}
	raw, _ := os.ReadFile(statusPath(h.cfg.Path))
	if strings.Contains(string(raw), testToken) {
		t.Fatal("status.json 不得含 token")
	}
	h.send(6, 10)
	h.waitUntil("版本過舊仍持續轉送", 0, func() bool { return h.accepted() == 10 })
}
