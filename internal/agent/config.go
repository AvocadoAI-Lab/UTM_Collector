package agent

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Agent     AgentConfig     `toml:"agent"`
	Listener  ListenerConfig  `toml:"listener"`
	Forwarder ForwarderConfig `toml:"forwarder"`
	Heartbeat HeartbeatConfig `toml:"heartbeat"`
	Spool     SpoolConfig     `toml:"spool"`
	Logging   LoggingConfig   `toml:"logging"`
	Proxy     ProxyConfig     `toml:"proxy"`

	// Path 為設定檔路徑（status.json 寫在同目錄），不從 TOML 讀入。
	Path string `toml:"-"`
}

type AgentConfig struct {
	AgentID string `toml:"agent_id"`
	SiteID  string `toml:"site_id"`
}

type ListenerConfig struct {
	Addr string `toml:"addr"`
	Port int    `toml:"port"`
}

type ForwarderConfig struct {
	Endpoint             string `toml:"endpoint"`
	Token                string `toml:"token"`
	BatchSize            int    `toml:"batch_size"`
	BatchIntervalSeconds int    `toml:"batch_interval_seconds"`
	// CAFile 為額外信任的 CA 憑證（PEM），用於私有 CA 或 mock server。不會跳過憑證驗證。
	CAFile string `toml:"ca_file"`
}

type HeartbeatConfig struct {
	IntervalSeconds int `toml:"interval_seconds"`
}

type SpoolConfig struct {
	Dir           string `toml:"dir"`
	RetentionDays int    `toml:"retention_days"`
	MaxSizeGB     int    `toml:"max_size_gb"`
}

type LoggingConfig struct {
	Level string `toml:"level"`
	Dir   string `toml:"dir"`
}

type ProxyConfig struct {
	URL string `toml:"url"`
}

var (
	siteIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

func DefaultConfigPath() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "pico-utm-agent", "config.toml")
	}
	return "/etc/pico-utm-agent/config.toml"
}

// LoadConfig 讀取並驗證設定檔。任何錯誤（含未知欄位）都拒絕啟動，不套用預設值。
func LoadConfig(path string) (*Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("讀取設定檔 %s 失敗：%w", path, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		names := make([]string, len(keys))
		for i, k := range keys {
			names[i] = k.String()
		}
		return nil, fmt.Errorf("設定檔 %s 含有未知欄位（可能拼錯）：%s", path, strings.Join(names, ", "))
	}
	cfg.Path = path
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	var errs []string
	bad := func(field, format string, a ...any) {
		errs = append(errs, field+"："+fmt.Sprintf(format, a...))
	}
	intRange := func(field string, v, lo, hi int) {
		if v < lo || v > hi {
			bad(field, "必須介於 %d–%d（目前為 %d）", lo, hi, v)
		}
	}

	if !uuidRe.MatchString(c.Agent.AgentID) {
		bad("agent.agent_id", "必須為 UUID 格式（目前為 %q）", c.Agent.AgentID)
	}
	if !siteIDRe.MatchString(c.Agent.SiteID) {
		bad("agent.site_id", "必須符合 ^[a-z0-9][a-z0-9-]{2,63}$（目前為 %q）", c.Agent.SiteID)
	}
	if net.ParseIP(c.Listener.Addr) == nil {
		bad("listener.addr", "必須為合法 IP 位址（目前為 %q）", c.Listener.Addr)
	}
	intRange("listener.port", c.Listener.Port, 1024, 65535)
	if err := validateEndpoint(c.Forwarder.Endpoint); err != nil {
		bad("forwarder.endpoint", "%v（目前為 %q）", err, c.Forwarder.Endpoint)
	}
	if strings.TrimSpace(c.Forwarder.Token) == "" {
		bad("forwarder.token", "不可為空")
	}
	intRange("forwarder.batch_size", c.Forwarder.BatchSize, 1, 10000)
	intRange("forwarder.batch_interval_seconds", c.Forwarder.BatchIntervalSeconds, 5, 300)
	if c.Forwarder.CAFile != "" {
		if _, err := os.Stat(c.Forwarder.CAFile); err != nil {
			bad("forwarder.ca_file", "無法讀取：%v", err)
		}
	}
	intRange("heartbeat.interval_seconds", c.Heartbeat.IntervalSeconds, 60, 3600)
	if strings.TrimSpace(c.Spool.Dir) == "" {
		bad("spool.dir", "不可為空")
	}
	intRange("spool.retention_days", c.Spool.RetentionDays, 1, 365)
	intRange("spool.max_size_gb", c.Spool.MaxSizeGB, 1, 500)
	if strings.TrimSpace(c.Logging.Dir) == "" {
		bad("logging.dir", "不可為空")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		bad("logging.level", "必須為 debug / info / warn / error（目前為 %q）", c.Logging.Level)
	}
	if c.Proxy.URL != "" {
		// 不回顯 proxy URL，避免帳密出現在輸出中
		u, err := url.Parse(c.Proxy.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("proxy.url", "必須為 http:// 或 https:// 開頭的合法 URL")
		}
	}

	if len(errs) > 0 {
		return errors.New("設定檔驗證失敗：\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

func validateEndpoint(s string) error {
	if !strings.HasPrefix(s, "https://") {
		return errors.New("必須以 https:// 開頭")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("URL 不合法：%v", err)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("URL 不合法（需有主機名稱，且不可含帳密、查詢字串或 #）")
	}
	return nil
}

func (c *Config) endpoint() string          { return strings.TrimRight(c.Forwarder.Endpoint, "/") }
func (c *Config) batchInterval() time.Duration { return time.Duration(c.Forwarder.BatchIntervalSeconds) * time.Second }
func (c *Config) heartbeatInterval() time.Duration {
	return time.Duration(c.Heartbeat.IntervalSeconds) * time.Second
}

// ConfigSummary 為寫入 status.json 的設定摘要，不含 token。
type ConfigSummary struct {
	AgentID                  string `json:"agent_id"`
	SiteID                   string `json:"site_id"`
	Listen                   string `json:"listen"`
	Endpoint                 string `json:"endpoint"`
	BatchSize                int    `json:"batch_size"`
	BatchIntervalSeconds     int    `json:"batch_interval_seconds"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
	SpoolDir                 string `json:"spool_dir"`
	RetentionDays            int    `json:"retention_days"`
	MaxSizeGB                int    `json:"max_size_gb"`
	LogDir                   string `json:"log_dir"`
	LogLevel                 string `json:"log_level"`
	ProxyURL                 string `json:"proxy_url"`
	CAFile                   string `json:"ca_file"`
}

func (c *Config) Summary() ConfigSummary {
	return ConfigSummary{
		AgentID:                  c.Agent.AgentID,
		SiteID:                   c.Agent.SiteID,
		Listen:                   net.JoinHostPort(c.Listener.Addr, fmt.Sprint(c.Listener.Port)),
		Endpoint:                 c.Forwarder.Endpoint,
		BatchSize:                c.Forwarder.BatchSize,
		BatchIntervalSeconds:     c.Forwarder.BatchIntervalSeconds,
		HeartbeatIntervalSeconds: c.Heartbeat.IntervalSeconds,
		SpoolDir:                 c.Spool.Dir,
		RetentionDays:            c.Spool.RetentionDays,
		MaxSizeGB:                c.Spool.MaxSizeGB,
		LogDir:                   c.Logging.Dir,
		LogLevel:                 c.Logging.Level,
		ProxyURL:                 redactURL(c.Proxy.URL),
		CAFile:                   c.Forwarder.CAFile,
	}
}
