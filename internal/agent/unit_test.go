package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// ---- Syslog 解析 ----

func TestParseUserAct(t *testing.T) {
	raw := `<14>Sep 11 16:05:59 Pico-UTM-A8C24600C6B2-MyPico user_act[2554]: {"ts":1789113959,"user":"admin","src":"192.168.1.200","msg":"Sign in"}`
	rec := BuildRecord([]byte(raw), "192.168.1.198", time.Date(2026, 9, 11, 8, 5, 59, 123456000, time.UTC))
	if rec.ParseError != nil {
		t.Fatalf("parse_error = %q", *rec.ParseError)
	}
	p := rec.Parsed
	if p.Program != "user_act" || p.Hostname != "Pico-UTM-A8C24600C6B2-MyPico" || *p.PID != 2554 || p.Priority != 14 {
		t.Fatalf("解析結果錯誤：%+v", p)
	}
	if rec.ReceivedAt != "2026-09-11T08:05:59.123456Z" {
		t.Fatalf("received_at = %s", rec.ReceivedAt)
	}
	if string(p.Payload) != `{"ts":1789113959,"user":"admin","src":"192.168.1.200","msg":"Sign in"}` {
		t.Fatalf("payload 未原樣保留：%s", p.Payload)
	}
}

func TestParseDPIEventKeepsAllFields(t *testing.T) {
	payload := `{"action":"Rdpage","dip":"2a00:1828:3000:0000:0000:0001:73ad:0002","domain":"secure.eicar.org","event_id":"178911518600003","module":"wg","sip":"2001:b011:0012:5820:d925:1418:c1d5:d2a3","ts":"1789115186","unknown_future_field":{"nested":[1,2,3]}}`
	rec := BuildRecord([]byte(`<14>Sep 11 16:26:26 Pico-UTM-A8C24600C6B2-MyPico dpi_event[2554]: `+payload), "192.168.1.198", time.Now())
	if rec.ParseError != nil {
		t.Fatalf("parse_error = %q", *rec.ParseError)
	}
	if string(rec.Parsed.Payload) != payload {
		t.Fatalf("payload 未原樣保留")
	}
	line, _ := MarshalRecord(rec)
	if !bytes.Contains(line, []byte(`"raw":"<14>Sep 11`)) {
		t.Fatalf("raw 不應被 HTML 跳脫：%s", line)
	}
	if eventIDOf(bytes.TrimSpace(line)) != "178911518600003" {
		t.Fatal("eventIDOf 取值錯誤")
	}
}

func TestParseMissingFields(t *testing.T) {
	// 無 PID、payload 缺少 event_id 等欄位：皆不應視為錯誤
	rec := BuildRecord([]byte(`<14>Sep  1 01:02:03 host dpi_event: {"module":"av"}`), "10.0.0.1", time.Now())
	if rec.ParseError != nil {
		t.Fatalf("parse_error = %q", *rec.ParseError)
	}
	if rec.Parsed.PID != nil || rec.Parsed.Program != "dpi_event" {
		t.Fatalf("解析結果錯誤：%+v", rec.Parsed)
	}
}

func TestParseNonJSONPayloadKeepsRecord(t *testing.T) {
	// 實機觀察到的 rapid 訊息：payload 非 JSON 且含換行
	raw := "<11>Sep 11 16:47:56 Pico-UTM-A8C24600C6B2-MyPico rapid[2554]: Abort 404 NotFoundError: from 127.0.0.1 [system] GET /apis/v1/cloud_scanned_devices/904748AF39EB\nDevice not found"
	rec := BuildRecord([]byte(raw), "192.168.1.198", time.Now())
	if rec.ParseError == nil || !strings.Contains(*rec.ParseError, "JSON") {
		t.Fatalf("應標記 parse_error，實際：%v", rec.ParseError)
	}
	if rec.Parsed == nil || rec.Parsed.Program != "rapid" || rec.Parsed.Payload != nil || rec.Raw != raw {
		t.Fatalf("應保留標頭解析結果與原始內容：%+v", rec)
	}
	line, _ := MarshalRecord(rec)
	if bytes.Count(line, []byte{'\n'}) != 1 || !json.Valid(bytes.TrimSpace(line)) {
		t.Fatalf("spool 行必須是單行合法 JSON：%q", line)
	}
	if !bytes.Contains(line, []byte(`"payload":null`)) {
		t.Fatalf("payload 應為 null：%s", line)
	}
}

func TestParseUnknownProgramNotDropped(t *testing.T) {
	rec := BuildRecord([]byte(`<13>Sep 11 16:00:00 host brand_new_prog[1]: {"a":1}`), "1.2.3.4", time.Now())
	if rec.ParseError != nil || rec.Parsed.Program != "brand_new_prog" {
		t.Fatalf("未知 program 應正常解析：%+v", rec)
	}
}

func TestParseBadHeader(t *testing.T) {
	for _, raw := range []string{"", "hello world", "<999>Sep 11 16:00:00 host p: {}", "<14>not a timestamp host p: {}", "<14>Sep 11 16:00:00 host"} {
		rec := BuildRecord([]byte(raw), "1.2.3.4", time.Now())
		if rec.ParseError == nil || rec.Parsed != nil || rec.Raw != raw {
			t.Errorf("%q：應 parsed=null 並保留 raw，實際 %+v", raw, rec)
		}
	}
}

func TestParseLongMessage(t *testing.T) {
	long := strings.Repeat("x", 64000)
	raw := `<14>Sep 11 16:00:00 host dpi_event[1]: {"msg":"` + long + `"}`
	rec := BuildRecord([]byte(raw), "1.2.3.4", time.Now())
	if rec.ParseError != nil || len(rec.Raw) != len(raw) {
		t.Fatal("超長訊息應完整保留")
	}
}

func TestParseNonUTF8(t *testing.T) {
	raw := append([]byte(`<14>Sep 11 16:00:00 host user_act[1]: {"msg":"`), 0xff, 0xfe, '"', '}')
	rec := BuildRecord(raw, "1.2.3.4", time.Now())
	if rec.ParseError == nil || rec.RawBase64 == "" {
		t.Fatalf("非 UTF-8 應標記 parse_error 並保留 raw_base64：%+v", rec)
	}
	line, err := MarshalRecord(rec)
	if err != nil || !json.Valid(bytes.TrimSpace(line)) {
		t.Fatalf("輸出必須是合法 JSON：%v %q", err, line)
	}
}

// ---- 退避 ----

func TestBackoffStepsWithJitter(t *testing.T) {
	cases := []struct {
		r    float64
		want []time.Duration
	}{
		{0, []time.Duration{800 * time.Millisecond, 4 * time.Second, 12 * time.Second, 24 * time.Second, 48 * time.Second, 48 * time.Second}},
		{0.5, []time.Duration{1 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second, 60 * time.Second}},
	}
	for _, c := range cases {
		b := &Backoff{rand: func() float64 { return c.r }}
		for i, want := range c.want {
			if got := b.Next(); got != want {
				t.Errorf("r=%v 第 %d 次：%s，預期 %s", c.r, i+1, got, want)
			}
		}
	}
	b := &Backoff{}
	for i := 0; i < 2000; i++ {
		base := backoffSteps[min(i%8, len(backoffSteps)-1)]
		if i%8 == 0 {
			b.Reset()
		}
		d := b.Next()
		if d < base*8/10 || d > base*12/10 {
			t.Fatalf("jitter 超出 ±20%%：base=%s got=%s", base, d)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	if d, ok := parseRetryAfter("7", now); !ok || d != 7*time.Second {
		t.Errorf("秒數：%v %v", d, ok)
	}
	if d, ok := parseRetryAfter("Fri, 11 Sep 2026 08:00:30 GMT", now); !ok || d != 30*time.Second {
		t.Errorf("HTTP 日期：%v %v", d, ok)
	}
	if _, ok := parseRetryAfter("", now); ok {
		t.Error("空值應回傳 false")
	}
	if d, _ := parseRetryAfter("999999", now); d != time.Hour {
		t.Errorf("應限制上限一小時：%v", d)
	}
}

// ---- redact ----

func TestRedactRemovesToken(t *testing.T) {
	const token = `s3cr3t"tok\en`
	r := NewRedactor(token, "proxypass")
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&redactWriter{w: &buf, r: r}, nil))
	log.Info("token="+token, "token", token, "err", errors.New("Authorization: Bearer "+token),
		"nested", map[string]string{"t": token}, "proxy", "http://user:proxypass@proxy:3128")
	log.Warn("unknown bearer", "header", "Bearer some-other-token")
	out := buf.String()
	for _, leak := range []string{token, `s3cr3t\"tok\\en`, "proxypass", "some-other-token"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log 中出現機密 %q：\n%s", leak, out)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("redact 後 log 行應仍為合法 JSON：%s", line)
		}
	}
	if got := redactURL("http://user:pass@proxy.corp.local:3128"); strings.Contains(got, "pass@") {
		t.Fatalf("proxy 密碼未遮蔽：%s", got)
	}
}

func TestConfigSummaryHasNoToken(t *testing.T) {
	cfg := validConfig(t)
	cfg.Proxy.URL = "http://u:proxypw@proxy:3128"
	data, _ := json.Marshal(cfg.Summary())
	if strings.Contains(string(data), cfg.Forwarder.Token) || strings.Contains(string(data), "proxypw") {
		t.Fatalf("設定摘要含有機密：%s", data)
	}
}

// ---- 設定 ----

const validTOML = `
[agent]
agent_id = "550e8400-e29b-41d4-a716-446655440000"
site_id  = "acme-taipei-hq"
[listener]
addr = "0.0.0.0"
port = 5514
[forwarder]
endpoint = "https://collector.example.com"
token = "secret-token"
batch_size = 500
batch_interval_seconds = 60
[heartbeat]
interval_seconds = 300
[spool]
dir = "spool"
retention_days = 30
max_size_gb = 10
[logging]
level = "info"
dir = "logs"
[proxy]
url = ""
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func validConfig(t *testing.T) *Config {
	cfg, err := LoadConfig(writeConfig(t, validTOML))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestConfigValidation(t *testing.T) {
	validConfig(t)
	cases := []struct{ from, to, field string }{
		{"port = 5514", "port = 514", "listener.port"},
		{`endpoint = "https://collector.example.com"`, `endpoint = "http://collector.example.com"`, "forwarder.endpoint"},
		{`endpoint = "https://collector.example.com"`, `endpoint = "https://"`, "forwarder.endpoint"},
		{`token = "secret-token"`, `token = ""`, "forwarder.token"},
		{`site_id  = "acme-taipei-hq"`, `site_id = "Acme"`, "agent.site_id"},
		{"batch_size = 500", "batch_size = 0", "forwarder.batch_size"},
		{"batch_interval_seconds = 60", "batch_interval_seconds = 301", "forwarder.batch_interval_seconds"},
		{"interval_seconds = 300", "interval_seconds = 59", "heartbeat.interval_seconds"},
		{"retention_days = 30", "retention_days = 366", "spool.retention_days"},
		{"max_size_gb = 10", "max_size_gb = 0", "spool.max_size_gb"},
		{"max_size_gb = 10\n", "", "spool.max_size_gb"}, // 缺少欄位不得靜默套用預設值
		{`url = ""`, `url = "ftp://x"`, "proxy.url"},
		{"batch_size = 500", "batch_szie = 500", "batch_szie"}, // 拼錯的欄位
	}
	for _, c := range cases {
		_, err := LoadConfig(writeConfig(t, strings.Replace(validTOML, c.from, c.to, 1)))
		if err == nil || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s → %s：應指出 %s，實際 %v", c.from, c.to, c.field, err)
		}
	}
}

// ---- spool 與 offset ----

func spoolLine(id int) []byte {
	line, _ := MarshalRecord(BuildRecord(dpiEvent(id), "192.168.1.198", time.Now()))
	return line
}

func TestOffsetAtomicUpdate(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSpool(dir, discardLog)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 11, 1, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		if err := s.Append(day, spoolLine(i)); err != nil {
			t.Fatal(err)
		}
	}
	lines, end, err := s.ReadBatch(2)
	if err != nil || len(lines) != 2 {
		t.Fatalf("ReadBatch：%d 筆，%v", len(lines), err)
	}
	if err := s.Commit(end); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".offset.tmp")); !os.IsNotExist(err) {
		t.Fatal(".offset.tmp 應已被 rename")
	}
	s.Close()

	// 模擬 rename 前斷電留下的暫存檔：不得影響讀取
	os.WriteFile(filepath.Join(dir, ".offset.tmp"), []byte("garbage"), 0o600)
	s2, _ := OpenSpool(dir, discardLog)
	if s2.Committed() != end {
		t.Fatalf("重新載入 offset = %+v，預期 %+v", s2.Committed(), end)
	}
	lines, _, _ = s2.ReadBatch(10)
	if len(lines) != 1 || eventIDOf(lines[0]) != fmt.Sprintf("%015d", 3) {
		t.Fatalf("應從第 3 筆續讀，實際 %d 筆", len(lines))
	}
	s2.Close()

	// offset 檔損壞：保守地從頭重送
	os.WriteFile(filepath.Join(dir, ".offset"), []byte("{broken"), 0o600)
	s3, _ := OpenSpool(dir, discardLog)
	defer s3.Close()
	if lines, _, _ := s3.ReadBatch(10); len(lines) != 3 {
		t.Fatalf("offset 損壞時應從頭讀取 3 筆，實際 %d", len(lines))
	}
}

func TestSpoolRepairsPartialLastLine(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "spool-2026-09-11.jsonl")
	content := append(append(spoolLine(1), spoolLine(2)...), []byte(`{"received_at":"2026-09-11T0`)...)
	os.WriteFile(name, content, 0o640)
	s, _ := OpenSpool(dir, discardLog)
	defer s.Close()
	st, _ := os.Stat(name)
	if st.Size() != int64(len(spoolLine(1))+len(spoolLine(2))) {
		t.Fatalf("殘缺行未被截斷，檔案大小 %d", st.Size())
	}
	if lines, _, _ := s.ReadBatch(10); len(lines) != 2 {
		t.Fatalf("應讀到 2 筆，實際 %d", len(lines))
	}
}

func TestSpoolSkipsCorruptLinesAndCrossesFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "spool-2026-09-11.jsonl"), append(append(spoolLine(1), "\x00\x00garbage\n"...), spoolLine(2)...), 0o640)
	os.WriteFile(filepath.Join(dir, "spool-2026-09-12.jsonl"), spoolLine(3), 0o640)
	s, _ := OpenSpool(dir, discardLog)
	defer s.Close()
	lines, end, _ := s.ReadBatch(10)
	if len(lines) != 3 || end.File != "spool-2026-09-12.jsonl" {
		t.Fatalf("應跨檔讀到 3 筆並停在第二個檔，實際 %d 筆 %+v", len(lines), end)
	}
}

// ---- tail 跨日 ----

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("逾時等待：%s", desc)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func appendFile(t *testing.T, path string, data []byte) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(data)
	f.Close()
}

func TestTailFollowsDayRollover(t *testing.T) {
	dir := t.TempDir()
	day1 := filepath.Join(dir, "spool-2026-09-11.jsonl")
	day2 := filepath.Join(dir, "spool-2026-09-12.jsonl")
	appendFile(t, day1, spoolLine(1)) // tail 啟動前已存在，不應輸出

	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Tail(ctx, dir, &out, 20*time.Millisecond)
	time.Sleep(200 * time.Millisecond)

	id := func(n int) string { return fmt.Sprintf("#%015d", n) }
	appendFile(t, day1, spoolLine(2))
	waitFor(t, "舊檔新事件", func() bool { return strings.Contains(out.String(), id(2)) })

	// 跨日：舊檔最後一筆與新檔第一筆幾乎同時寫入
	appendFile(t, day1, spoolLine(3))
	appendFile(t, day2, spoolLine(4))
	waitFor(t, "跨日前後的事件", func() bool { return strings.Contains(out.String(), id(3)) && strings.Contains(out.String(), id(4)) })
	appendFile(t, day2, spoolLine(5))
	waitFor(t, "新檔後續事件", func() bool { return strings.Contains(out.String(), id(5)) })

	if strings.Contains(out.String(), id(1)) {
		t.Fatal("不應輸出 tail 啟動前的事件")
	}
	if strings.Index(out.String(), id(3)) > strings.Index(out.String(), id(4)) {
		t.Fatal("跨日時應先輸出舊檔剩餘事件")
	}
}

// 刪除 forwarder 正讀到中段的 spool 檔時，尚未讀取的後半段必須計入 dropped。
func TestCleanupCountsUnreadRemainderAsDropped(t *testing.T) {
	cases := []struct {
		name     string
		inflight bool
		want     int64
	}{
		{"已送達 1–8，無轉送中批次", false, 12}, // 9–20 未送出
		{"已送達 1–8，9–13 轉送中", true, 7}, // 轉送中的仍會送達，14–20 未讀取
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for i := 1; i <= 20; i++ {
				appendFile(t, filepath.Join(dir, "spool-2026-09-08.jsonl"), spoolLine(i))
			}
			newest := filepath.Join(dir, "spool-2026-09-09.jsonl")
			appendFile(t, newest, spoolLine(21))
			s, _ := OpenSpool(dir, discardLog)
			defer s.Close()

			_, end, _ := s.ReadBatch(8)
			if err := s.Commit(end); err != nil {
				t.Fatal(err)
			}
			if c.inflight {
				if lines, _, _ := s.ReadBatch(5); len(lines) != 5 {
					t.Fatalf("轉送中批次應為 5 筆，實際 %d", len(lines))
				}
			}
			st, _ := os.Stat(newest)
			if got := s.Cleanup(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC), 30, st.Size()); got != c.want {
				t.Fatalf("dropped = %d，預期 %d", got, c.want)
			}
			if _, err := os.Stat(filepath.Join(dir, "spool-2026-09-08.jsonl")); !os.IsNotExist(err) {
				t.Fatal("中段檔案應已刪除")
			}
			// 刪除後 forwarder 應跳到下一個檔
			if lines, _, _ := s.ReadBatch(10); len(lines) != 1 || eventIDOf(lines[0]) != fmt.Sprintf("%015d", 21) {
				t.Fatalf("刪除後應從下一個檔續讀，實際 %d 筆", len(lines))
			}
		})
	}
}

func TestCleanupCountsUnsentPreview(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "spool-2026-09-08.jsonl")
	for i := 1; i <= 20; i++ {
		appendFile(t, old, spoolLine(i))
	}
	s, err := OpenSpool(dir, discardLog)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, end, err := s.ReadBatch(8)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(end); err != nil {
		t.Fatal(err)
	}
	if lines, _, err := s.ReadReadyBatch(500, false); err != nil || len(lines) != 0 {
		t.Fatalf("short batch must wait: len=%d err=%v", len(lines), err)
	}
	if got := s.Cleanup(time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), 30, 1<<30); got != 12 {
		t.Fatalf("unsent preview must count as dropped: got %d, want 12", got)
	}
	if got := s.Cleanup(time.Now(), 30, 1<<30); got != 0 {
		t.Fatalf("double counted: %d", got)
	}
}

func TestLastLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-2026-09-13.log")
	var sb strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&sb, `{"n":%d,"pad":"%s"}`+"\n", i, strings.Repeat("x", 50))
	}
	os.WriteFile(path, []byte(sb.String()), 0o640)
	lines, err := lastLines(path, 3)
	if err != nil || len(lines) != 3 || !strings.Contains(lines[0], `"n":4998`) || !strings.Contains(lines[2], `"n":5000`) {
		t.Fatalf("lastLines 錯誤：%v %q", err, lines)
	}
	if lines, _ := lastLines(path, 100000); len(lines) != 5000 {
		t.Fatalf("行數不足時應回傳全部，實際 %d", len(lines))
	}
}

func TestVersionLess(t *testing.T) {
	if !versionLess("1.0.0", "1.0.1") || !versionLess("1.9.0", "1.10.0") || versionLess("1.0.0", "1.0.0") || versionLess("2.0.0", "1.99.99") {
		t.Fatal("版本比較錯誤")
	}
}
