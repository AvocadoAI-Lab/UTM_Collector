package agent

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"
)

const (
	lowDiskBytes   = 1 << 30
	maxSourceIPs   = 32
	statusInterval = 10 * time.Second
	staleStatus    = 60 * time.Second
)

// Options 為非設定檔的注入項，主要供測試使用。
type Options struct {
	Clock         Clock          // 預設 RealClock
	Logger        *slog.Logger   // 預設不輸出
	RootCAs       *x509.CertPool // 測試用：信任 mock server 的自簽憑證
	MaxSpoolBytes int64          // 測試用：覆寫 spool.max_size_gb
}

type Agent struct {
	cfg   *Config
	clock Clock
	log   *slog.Logger
	spool *Spool
	api   *apiClient
	fwd   *Forwarder
	conn  *net.UDPConn
	stats Stats

	startedAt     time.Time
	maxSpoolBytes int64
	janitorMu     sync.Mutex
	lastDiskLow   time.Time
}

func New(cfg *Config, opts Options) (*Agent, error) {
	if opts.Clock == nil {
		opts.Clock = RealClock
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.MaxSpoolBytes == 0 {
		opts.MaxSpoolBytes = int64(cfg.Spool.MaxSizeGB) << 30
	}
	api, err := newAPIClient(cfg, opts.RootCAs)
	if err != nil {
		return nil, err
	}
	spool, err := OpenSpool(cfg.Spool.Dir, opts.Logger)
	if err != nil {
		return nil, err
	}
	conn, err := listenUDP(cfg.Listener.Addr, cfg.Listener.Port)
	if err != nil {
		spool.Close()
		return nil, fmt.Errorf("綁定 UDP %s:%d 失敗：%w", cfg.Listener.Addr, cfg.Listener.Port, err)
	}
	a := &Agent{
		cfg:           cfg,
		clock:         opts.Clock,
		log:           opts.Logger,
		spool:         spool,
		api:           api,
		conn:          conn,
		maxSpoolBytes: opts.MaxSpoolBytes,
	}
	a.fwd = &Forwarder{
		cfg: cfg, spool: spool, api: api, stats: &a.stats,
		clock: opts.Clock, log: opts.Logger, wake: make(chan struct{}, 1),
	}
	return a, nil
}

func listenUDP(addr string, port int) (*net.UDPConn, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(addr), Port: port})
	if err != nil {
		return nil, err
	}
	_ = conn.SetReadBuffer(4 << 20)
	return conn, nil
}

func (a *Agent) UDPAddr() *net.UDPAddr { return a.conn.LocalAddr().(*net.UDPAddr) }

// Run 啟動 receiver、forwarder、heartbeat 與背景工作，直到 ctx 結束。
func (a *Agent) Run(ctx context.Context) error {
	a.startedAt = a.clock.Now()
	proxy, _ := a.api.proxyFor()
	proxyStr := "直連"
	if proxy != nil {
		proxyStr = proxy.Redacted()
	}
	a.log.Info("agent 啟動", "version", Version, "site_id", a.cfg.Agent.SiteID, "listen", a.conn.LocalAddr().String(),
		"endpoint", a.cfg.Forwarder.Endpoint, "proxy", proxyStr, "offset", a.spool.Committed())

	var wg sync.WaitGroup
	run := func(fn func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); fn(ctx) }()
	}
	run(a.runReceiver)
	run(a.fwd.Run)
	run(a.runHeartbeat)
	run(every(time.Second, func() {
		if err := a.spool.Sync(); err != nil {
			a.log.Error("spool fsync 失敗", "error", err)
		}
	}))
	run(every(time.Minute, a.runJanitor))
	run(every(statusInterval, a.writeStatus))

	<-ctx.Done()
	wg.Wait()
	if err := a.spool.Close(); err != nil {
		a.log.Error("關閉 spool 失敗", "error", err)
	}
	a.writeStatus()
	a.log.Info("agent 已停止")
	return nil
}

// every 立即執行一次 fn，之後每隔 d 執行，直到 ctx 結束。
func every(d time.Duration, fn func()) func(context.Context) {
	return func(ctx context.Context) {
		fn()
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				fn()
			}
		}
	}
}

// runReceiver 只做收封包、解析、寫入 spool；不做任何網路 I/O。
func (a *Agent) runReceiver(ctx context.Context) {
	go func() { <-ctx.Done(); a.conn.Close() }()
	buf := make([]byte, 65536)
	var failed int
	var lastErrLog time.Time
	for {
		n, src, err := a.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			a.log.Warn("UDP 接收錯誤", "error", err)
			continue
		}
		now := a.clock.Now()
		srcIP := src.IP.String()
		rec := BuildRecord(buf[:n], srcIP, now)
		line, err := MarshalRecord(rec)
		if err == nil {
			err = a.spool.Append(now, line)
		}
		a.stats.update(func(v *statsValues) {
			v.received++
			v.lastEventReceivedAt = rec.ReceivedAt
			if !slices.Contains(v.sourceIPs, srcIP) && len(v.sourceIPs) < maxSourceIPs {
				v.sourceIPs = append(v.sourceIPs, srcIP)
			}
			if err != nil {
				v.spoolWriteErrors++
			}
		})
		if err != nil {
			failed++
			if time.Since(lastErrLog) >= 10*time.Second {
				a.log.Error("事件寫入 spool 失敗（磁碟可能已滿），事件遺失", "error", err, "failed_since_last_log", failed)
				failed, lastErrLog = 0, time.Now()
			}
			continue
		}
		a.fwd.Notify()
	}
}

func (a *Agent) runJanitor() {
	a.janitorMu.Lock()
	defer a.janitorMu.Unlock()
	if dropped := a.spool.Cleanup(a.clock.Now(), a.cfg.Spool.RetentionDays, a.maxSpoolBytes); dropped > 0 {
		a.stats.update(func(v *statsValues) { v.dropped += dropped })
	}
	if free, err := DiskFree(a.cfg.Spool.Dir); err == nil && free < lowDiskBytes && time.Since(a.lastDiskLow) >= 10*time.Minute {
		a.log.Error("磁碟可用空間低於 1GB", "dir", a.cfg.Spool.Dir, "free_bytes", free)
		a.lastDiskLow = time.Now()
	}
}

// ---- 統計、心跳與狀態檔 ----

type Stats struct {
	mu sync.Mutex
	v  statsValues
}

type statsValues struct {
	received, forwarded, deadLettered, dropped, spoolWriteErrors int64
	lastEventReceivedAt                                          string
	lastForwardSuccessAt, lastForwardError                       string
	lastHeartbeatAt, lastHeartbeatError                          string
	minSupportedVersion                                          string
	sourceIPs                                                    []string
}

func (s *Stats) update(fn func(v *statsValues)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.v)
}

func (s *Stats) snapshot() statsValues {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.v
	v.sourceIPs = slices.Clone(s.v.sourceIPs)
	return v
}

type HeartbeatPayload struct {
	AgentID                 string   `json:"agent_id"`
	SiteID                  string   `json:"site_id"`
	AgentVersion            string   `json:"agent_version"`
	Hostname                string   `json:"hostname"`
	OS                      string   `json:"os"`
	SentAt                  string   `json:"sent_at"`
	UptimeSeconds           int64    `json:"uptime_seconds"`
	LastEventReceivedAt     *string  `json:"last_event_received_at"`
	EventsReceivedTotal     int64    `json:"events_received_total"`
	EventsForwardedTotal    int64    `json:"events_forwarded_total"`
	EventsDeadLetteredTotal int64    `json:"events_dead_lettered_total"`
	EventsDroppedTotal      int64    `json:"events_dropped_total"`       // 因 spool 清理而刪除、尚未轉送的事件數
	SpoolWriteErrorsTotal   int64    `json:"spool_write_errors_total"`   // 寫入 spool 失敗（例如磁碟已滿）的事件數
	SpoolBacklogBytes       int64    `json:"spool_backlog_bytes"`
	SpoolTotalBytes         int64    `json:"spool_total_bytes"`
	SpoolDiskFreeBytes      int64    `json:"spool_disk_free_bytes"`
	LastForwardSuccessAt    *string  `json:"last_forward_success_at"`
	LastForwardError        *string  `json:"last_forward_error"`
	ProxyInUse              bool     `json:"proxy_in_use"`
	UTMSourceIPs            []string `json:"utm_source_ips"`
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (a *Agent) heartbeatPayload() HeartbeatPayload {
	v := a.stats.snapshot()
	backlog, total := a.spool.Sizes()
	free, _ := DiskFree(a.cfg.Spool.Dir)
	host, _ := os.Hostname()
	proxy, _ := a.api.proxyFor()
	now := a.clock.Now()
	ips := v.sourceIPs
	if ips == nil {
		ips = []string{}
	}
	return HeartbeatPayload{
		AgentID:                 a.cfg.Agent.AgentID,
		SiteID:                  a.cfg.Agent.SiteID,
		AgentVersion:            Version,
		Hostname:                host,
		OS:                      runtime.GOOS + "/" + runtime.GOARCH,
		SentAt:                  formatMillis(now),
		UptimeSeconds:           int64(now.Sub(a.startedAt).Seconds()),
		LastEventReceivedAt:     nullable(v.lastEventReceivedAt),
		EventsReceivedTotal:     v.received,
		EventsForwardedTotal:    v.forwarded,
		EventsDeadLetteredTotal: v.deadLettered,
		EventsDroppedTotal:      v.dropped,
		SpoolWriteErrorsTotal:   v.spoolWriteErrors,
		SpoolBacklogBytes:       backlog,
		SpoolTotalBytes:         total,
		SpoolDiskFreeBytes:      free,
		LastForwardSuccessAt:    nullable(v.lastForwardSuccessAt),
		LastForwardError:        nullable(v.lastForwardError),
		ProxyInUse:              proxy != nil,
		UTMSourceIPs:            ips,
	}
}

// runHeartbeat 獨立於 forwarder：啟動時送一次，之後依間隔送出；失敗就等下一次。
func (a *Agent) runHeartbeat(ctx context.Context) {
	a.sendHeartbeat(ctx)
	t := time.NewTicker(a.cfg.heartbeatInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.sendHeartbeat(ctx)
		}
	}
}

func (a *Agent) sendHeartbeat(ctx context.Context) {
	payload := a.heartbeatPayload()
	res, err := a.api.post(ctx, "/heartbeat", payload)
	if ctx.Err() != nil {
		return
	}
	var failure string
	switch {
	case err != nil:
		failure = fmt.Sprintf("連線失敗：%v", err)
	case res.status < 200 || res.status >= 300:
		failure = fmt.Sprintf("HTTP %d", res.status)
	}
	if failure != "" {
		a.stats.update(func(v *statsValues) { v.lastHeartbeatError = failure })
		a.log.Warn("心跳送出失敗", "error", failure)
		return
	}

	var body struct {
		MinSupportedVersion string `json:"min_supported_version"`
	}
	_ = json.Unmarshal(res.body, &body)
	a.stats.update(func(v *statsValues) {
		v.lastHeartbeatAt = payload.SentAt
		v.lastHeartbeatError = ""
		if body.MinSupportedVersion != "" {
			v.minSupportedVersion = body.MinSupportedVersion
		}
	})
	a.log.Info("心跳送出成功", "status", res.status, "backlog_bytes", payload.SpoolBacklogBytes,
		"events_received_total", payload.EventsReceivedTotal)
	if body.MinSupportedVersion != "" && versionLess(Version, body.MinSupportedVersion) {
		a.log.Warn("目前版本低於接收端最低支援版本，請重新執行安裝腳本升級（服務照常運作）",
			"agent_version", Version, "min_supported_version", body.MinSupportedVersion)
	}
}

// StatusFile 為 status.json 的內容：心跳所有欄位，加上服務狀態與設定摘要（不含 token）。
type StatusFile struct {
	HeartbeatPayload
	StartedAt           string        `json:"started_at"`
	UpdatedAt           string        `json:"updated_at"`
	LastHeartbeatAt     *string       `json:"last_heartbeat_at"`
	LastHeartbeatError  *string       `json:"last_heartbeat_error"`
	MinSupportedVersion *string       `json:"min_supported_version"`
	UpgradeRequired     bool          `json:"upgrade_required"`
	Config              ConfigSummary `json:"config"`
}

func statusPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "status.json")
}

func (a *Agent) buildStatus() StatusFile {
	p := a.heartbeatPayload()
	v := a.stats.snapshot()
	return StatusFile{
		HeartbeatPayload:    p,
		StartedAt:           formatMillis(a.startedAt),
		UpdatedAt:           p.SentAt,
		LastHeartbeatAt:     nullable(v.lastHeartbeatAt),
		LastHeartbeatError:  nullable(v.lastHeartbeatError),
		MinSupportedVersion: nullable(v.minSupportedVersion),
		UpgradeRequired:     v.minSupportedVersion != "" && versionLess(Version, v.minSupportedVersion),
		Config:              a.cfg.Summary(),
	}
}

func (a *Agent) writeStatus() {
	data, err := json.MarshalIndent(a.buildStatus(), "", "  ")
	if err == nil {
		err = writeFileAtomic(statusPath(a.cfg.Path), data)
	}
	if err != nil {
		a.log.Warn("寫入 status.json 失敗", "error", err)
	}
}
