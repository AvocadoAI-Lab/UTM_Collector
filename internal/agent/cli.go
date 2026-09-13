package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 本檔為 agent test / status / tail 三個 CLI 命令的實作。

type checkPrinter struct {
	w      io.Writer
	failed int
}

func (p *checkPrinter) ok(format string, a ...any) {
	fmt.Fprintf(p.w, "[✓] "+format+"\n", a...)
}

func (p *checkPrinter) fail(msg string, advice ...string) {
	p.failed++
	fmt.Fprintf(p.w, "[✗] %s\n", msg)
	for _, s := range advice {
		fmt.Fprintf(p.w, "    %s\n", s)
	}
}

// SelfTest 逐項檢查部署環境，不修改任何狀態。回傳 exit code。
func SelfTest(ctx context.Context, configPath string, w io.Writer) int {
	p := &checkPrinter{w: w}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		p.fail("設定檔無法載入", strings.Split(err.Error(), "\n")...)
		return 1
	}
	p.ok("設定檔存在且格式正確（%s）", configPath)

	if wide, detail, err := ConfigPermissionTooWide(configPath); err != nil {
		p.fail("無法檢查設定檔權限：" + err.Error())
	} else if wide {
		p.fail("設定檔權限過寬："+detail, permissionAdvice(configPath)...)
	} else {
		p.ok("設定檔權限安全")
	}

	if err := checkWritable(cfg.Spool.Dir); err != nil {
		p.fail("Spool 目錄無法寫入："+err.Error(), "建議：確認目錄存在且服務帳號具寫入權限（重新執行安裝腳本會自動建立）")
	} else {
		p.ok("Spool 目錄可寫入（%s）", cfg.Spool.Dir)
	}

	if free, err := DiskFree(cfg.Spool.Dir); err != nil {
		p.fail("無法取得磁碟可用空間：" + err.Error())
	} else if free < lowDiskBytes {
		p.fail(fmt.Sprintf("磁碟可用空間不足 1GB（%s）", formatBytes(free)), "建議：清理磁碟或將 spool.dir 移到空間較大的磁碟")
	} else {
		p.ok("磁碟可用空間 %s", formatBytes(free))
	}

	if conn, err := listenUDP(cfg.Listener.Addr, cfg.Listener.Port); err == nil {
		conn.Close()
		p.ok("UDP %d 可綁定", cfg.Listener.Port)
	} else if st, err2 := readStatus(configPath); err2 == nil && statusAge(st) < staleStatus {
		p.ok("UDP %d 已由執行中的服務使用", cfg.Listener.Port)
	} else {
		p.fail(fmt.Sprintf("UDP %d 無法綁定：%v", cfg.Listener.Port, err),
			fmt.Sprintf("建議：確認沒有其他程式佔用此埠（netstat -ano | findstr :%d）", cfg.Listener.Port))
	}

	if api, err := newAPIClient(cfg, nil); err != nil {
		p.fail("建立 HTTPS 用戶端失敗：" + err.Error())
	} else {
		checkConnectivity(ctx, p, cfg, api)
	}

	checkFirewall(p, cfg.Listener.Port)

	if p.failed > 0 {
		fmt.Fprintf(w, "\n共 %d 項檢查未通過\n", p.failed)
		return 1
	}
	fmt.Fprintln(w, "\n所有檢查皆通過")
	return 0
}

func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// checkConnectivity 依序檢查 DNS → TCP → TLS → Token → 時間偏差，區分失敗原因。
func checkConnectivity(ctx context.Context, p *checkPrinter, cfg *Config, api *apiClient) {
	target, _ := url.Parse(cfg.endpoint())
	label := "端點"
	proxy, err := api.proxyFor()
	if err != nil {
		p.fail("proxy 設定錯誤：" + err.Error())
		return
	}
	if proxy != nil {
		source := "設定檔"
		if cfg.Proxy.URL == "" {
			source = "環境變數"
		}
		p.ok("經由 proxy 連線：%s（來源：%s）", proxy.Redacted(), source)
		target, label = proxy, "Proxy"
	} else {
		p.ok("直接連線（未使用 proxy）")
	}

	host, port := target.Hostname(), target.Port()
	if port == "" {
		port = map[string]string{"http": "80"}[target.Scheme]
		if port == "" {
			port = "443"
		}
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(dctx, host)
	if err != nil {
		p.fail(fmt.Sprintf("%s DNS 解析失敗（%s）：%v", label, host, err),
			"建議：確認網址拼字正確，並確認此主機的 DNS 設定可用（nslookup "+host+"）")
		return
	}
	p.ok("%s DNS 解析成功 (%s → %s)", label, host, strings.Join(addrs, ", "))

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 10*time.Second)
	if err != nil {
		p.fail(fmt.Sprintf("%s TCP 連線失敗（%s:%s）：%v", label, host, port, err),
			"建議：確認對外防火牆允許連線到此埠，且服務正在執行")
		return
	}
	conn.Close()
	p.ok("%s TCP 連線成功 (%s:%s)", label, host, port)

	res, err := api.get(ctx, "/healthz")
	if err != nil {
		if isCertError(err) {
			p.fail("TLS 憑證錯誤："+err.Error(),
				"建議：確認系統時間正確、網路中沒有 SSL 檢測設備替換憑證、端點憑證未過期且主機名稱相符")
		} else {
			p.fail("HTTPS 連線失敗："+err.Error(), "建議：確認端點網址與埠號正確；若經由 proxy，確認 proxy 允許 CONNECT 到端點")
		}
		return
	}
	p.ok("TLS 連線建立成功，憑證有效")

	switch {
	case res.status == 401:
		p.fail("Token 驗證失敗（HTTP 401）", "建議：確認 token 正確，或向接收端管理者確認 token 未被撤銷")
		return
	case res.status < 200 || res.status >= 300:
		p.fail(fmt.Sprintf("端點回應 HTTP %d：%s", res.status, summarizeBody(res.body)),
			"建議：確認 endpoint 為接收端的根網址（不含 /events）")
		return
	}
	p.ok("Token 驗證通過")

	var hz struct {
		ServerTime string `json:"server_time"`
	}
	serverTime, err := time.Time{}, json.Unmarshal(res.body, &hz)
	if err == nil {
		serverTime, err = time.Parse(time.RFC3339Nano, hz.ServerTime)
	}
	if err != nil {
		p.fail("無法從 /healthz 取得伺服器時間")
		return
	}
	skew := time.Since(serverTime).Seconds()
	if skew > 300 || skew < -300 {
		p.fail(fmt.Sprintf("時間偏差 %.1f 秒（超過 5 分鐘）", skew), "建議：校正系統時間（Windows：w32tm /resync；Linux：timedatectl）")
		return
	}
	p.ok("時間偏差 %.1f 秒", skew)
}

func isCertError(err error) bool {
	var cve *tls.CertificateVerificationError
	var uae x509.UnknownAuthorityError
	var he x509.HostnameError
	var cie x509.CertificateInvalidError
	return errors.As(err, &cve) || errors.As(err, &uae) || errors.As(err, &he) || errors.As(err, &cie)
}

// ---- agent status ----

func readStatus(configPath string) (StatusFile, error) {
	var st StatusFile
	data, err := os.ReadFile(statusPath(configPath))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(data, &st)
}

func statusAge(st StatusFile) time.Duration {
	t, err := time.Parse(time.RFC3339Nano, st.UpdatedAt)
	if err != nil {
		return time.Duration(1<<63 - 1)
	}
	return time.Since(t)
}

// PrintStatus 讀取 status.json 並格式化輸出。回傳 exit code。
func PrintStatus(configPath string, w io.Writer) int {
	st, err := readStatus(configPath)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(w, "找不到狀態檔 %s，服務可能未執行\n\n", statusPath(configPath))
		if cfg, cerr := LoadConfig(configPath); cerr == nil {
			printLogLocation(w, cfg.Logging.Dir)
		}
		return 1
	}
	if err != nil {
		fmt.Fprintf(w, "無法讀取狀態檔：%v\n", err)
		return 1
	}
	code := 0
	if age := statusAge(st); age > staleStatus {
		fmt.Fprintf(w, "⚠ 狀態檔已 %s 未更新，服務可能未執行\n\n", age.Round(time.Second))
		code = 1
	}
	orNone := func(s *string, none string) string {
		if s == nil {
			return none
		}
		return *s
	}
	rows := [][2]string{
		{"版本", st.AgentVersion},
		{"Agent ID", st.AgentID},
		{"Site ID", st.SiteID},
		{"主機", st.Hostname + "（" + st.OS + "）"},
		{"服務啟動時間", st.StartedAt},
		{"運行時間", (time.Duration(st.UptimeSeconds) * time.Second).String()},
		{"狀態更新時間", st.UpdatedAt},
		{"", ""},
		{"最後收到事件", orNone(st.LastEventReceivedAt, "尚未收到任何事件")},
		{"UTM 來源 IP", strings.Join(st.UTMSourceIPs, ", ")},
		{"收到事件", fmt.Sprint(st.EventsReceivedTotal)},
		{"已轉送", fmt.Sprint(st.EventsForwardedTotal)},
		{"Dead-letter", fmt.Sprint(st.EventsDeadLetteredTotal)},
		{"清理丟棄（未轉送）", fmt.Sprint(st.EventsDroppedTotal)},
		{"Spool 寫入失敗", fmt.Sprint(st.SpoolWriteErrorsTotal)},
		{"", ""},
		{"Spool 積壓", formatBytes(st.SpoolBacklogBytes)},
		{"Spool 總量", formatBytes(st.SpoolTotalBytes)},
		{"磁碟可用空間", formatBytes(st.SpoolDiskFreeBytes)},
		{"最後轉送成功", orNone(st.LastForwardSuccessAt, "無")},
		{"最後轉送錯誤", orNone(st.LastForwardError, "無")},
		{"最後心跳成功", orNone(st.LastHeartbeatAt, "無")},
		{"心跳錯誤", orNone(st.LastHeartbeatError, "無")},
		{"經由 proxy", map[bool]string{true: "是（" + st.Config.ProxyURL + "）", false: "否"}[st.ProxyInUse]},
		{"端點", st.Config.Endpoint},
		{"監聽", st.Config.Listen},
	}
	for _, r := range rows {
		if r[0] == "" {
			fmt.Fprintln(w)
			continue
		}
		fmt.Fprintf(w, "%s%s\n", padDisplay(r[0]+"：", 20), r[1])
	}
	fmt.Fprintln(w)
	printLogLocation(w, st.Config.LogDir)
	if st.SpoolDiskFreeBytes > 0 && st.SpoolDiskFreeBytes < lowDiskBytes {
		fmt.Fprintln(w, "\n⚠ 磁碟可用空間低於 1GB")
	}
	if st.UpgradeRequired && st.MinSupportedVersion != nil {
		fmt.Fprintf(w, "\n⚠ 目前版本 %s 低於接收端最低支援版本 %s，請重新執行安裝腳本升級\n", st.AgentVersion, *st.MinSupportedVersion)
	}
	return code
}

func printLogLocation(w io.Writer, dir string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	latest, err := LatestLogFile(dir)
	if err != nil {
		latest = "（尚無 log 檔）"
	}
	fmt.Fprintf(w, "%s%s\n", padDisplay("Log 目錄：", 20), abs)
	fmt.Fprintf(w, "%s%s\n", padDisplay("最新 Log 檔：", 20), latest)
	fmt.Fprintf(w, "%s%s\n", padDisplay("查看 log：", 20), "agent logs -n 50")
}

// padDisplay 依終端機顯示寬度補空白（中日韓與全形字元佔兩格）。
func padDisplay(s string, width int) string {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x2e80 && r <= 0xa4cf, r >= 0xac00 && r <= 0xd7a3, r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60, r >= 0xffe0 && r <= 0xffe6:
			w += 2
		default:
			w++
		}
	}
	return s + strings.Repeat(" ", max(width-w, 1))
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ---- agent tail ----

// Tail 追讀最新的 spool 檔並即時輸出事件摘要；跨日出現新檔時，先讀完舊檔剩餘內容再切換。
func Tail(ctx context.Context, dir string, w io.Writer, poll time.Duration) error {
	if _, err := os.Stat(dir); err != nil {
		return err
	}
	cur := latestSpool(dir)
	var off int64
	if cur != "" {
		if st, err := os.Stat(filepath.Join(dir, cur)); err == nil {
			off = st.Size()
		}
	}
	for {
		if cur != "" {
			off = tailRead(filepath.Join(dir, cur), off, w)
		}
		if latest := latestSpool(dir); latest > cur {
			if cur != "" {
				off = tailRead(filepath.Join(dir, cur), off, w)
			}
			cur, off = latest, 0
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(poll):
		}
	}
}

func latestSpool(dir string) string {
	var latest string
	for _, f := range listSpoolFiles(dir) {
		if !f.gz && f.name > latest {
			latest = f.name
		}
	}
	return latest
}

func tailRead(path string, off int64, w io.Writer) int64 {
	f, err := os.Open(path)
	if err != nil {
		return off
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() < off {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return off
	}
	r := bufio.NewReaderSize(f, 256*1024)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return off // EOF 或殘缺行：留待下次
		}
		off += int64(len(line))
		fmt.Fprintln(w, formatTailLine(line))
	}
}

func formatTailLine(line []byte) string {
	var r struct {
		ReceivedAt string `json:"received_at"`
		SourceIP   string `json:"source_ip"`
		Raw        string `json:"raw"`
		Parsed     *struct {
			Program string         `json:"program"`
			Payload map[string]any `json:"payload"`
		} `json:"parsed"`
		ParseError *string `json:"parse_error"`
	}
	if json.Unmarshal(line, &r) != nil {
		return "（無法解析的 spool 行）"
	}
	ts := r.ReceivedAt
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		ts = t.Local().Format("2006-01-02 15:04:05.000")
	}
	program, summary := "-", ""
	if r.Parsed != nil {
		program = r.Parsed.Program
		pl := r.Parsed.Payload
		str := func(k string) string {
			if v, ok := pl[k]; ok && v != nil {
				return fmt.Sprint(v)
			}
			return ""
		}
		var parts []string
		if _, ok := pl["module"]; ok {
			for _, k := range []string{"module", "action", "severity", "domain"} {
				if s := str(k); s != "" {
					parts = append(parts, s)
				}
			}
			summary = strings.Join(parts, " / ")
			if id := str("event_id"); id != "" {
				summary += "  #" + id
			}
			if msg := str("msg"); msg != "" {
				summary += "  " + msg
			}
		} else {
			for _, k := range []string{"user", "src", "msg"} {
				if s := str(k); s != "" {
					parts = append(parts, k+"="+s)
				}
			}
			summary = strings.Join(parts, " ")
		}
	}
	if r.ParseError != nil {
		raw := strings.Join(strings.Fields(r.Raw), " ")
		if len(raw) > 120 {
			raw = raw[:120] + "…"
		}
		summary = strings.TrimSpace(summary + "  [parse_error] " + *r.ParseError + "：" + raw)
	}
	return fmt.Sprintf("%s  %-15s  %-10s  %s", ts, r.SourceIP, program, summary)
}
