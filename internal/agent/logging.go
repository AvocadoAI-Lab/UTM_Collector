package agent

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const logRetentionDays = 30

// NewLogger 建立 JSON 格式、按日輪替的 logger。所有輸出都先經過 redact 再寫出。
// console 不為 nil 時同時輸出到 console（前景執行用）。
func NewLogger(cfg *Config, console io.Writer) (*slog.Logger, io.Closer, error) {
	if err := os.MkdirAll(cfg.Logging.Dir, 0o750); err != nil {
		return nil, nil, err
	}
	df := &dailyFile{dir: cfg.Logging.Dir}
	var w io.Writer = df
	if console != nil {
		w = io.MultiWriter(df, console)
	}
	red := NewRedactor(cfg.Forwarder.Token, urlPassword(cfg.Proxy.URL))
	h := slog.NewJSONHandler(&redactWriter{w: w, r: red}, &slog.HandlerOptions{Level: parseLevel(cfg.Logging.Level)})
	return slog.New(h), df, nil
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// dailyFile 寫入 agent-YYYY-MM-DD.log，換日時切檔並刪除超過 30 天的 log。
type dailyFile struct {
	dir  string
	mu   sync.Mutex
	date string
	f    *os.File
}

func (d *dailyFile) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if today := now.Format(dateLayout); d.f == nil || today != d.date {
		if d.f != nil {
			d.f.Close()
			d.f = nil
		}
		f, err := os.OpenFile(filepath.Join(d.dir, "agent-"+today+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return 0, err
		}
		d.f, d.date = f, today
		d.cleanup(now)
	}
	return d.f.Write(p)
}

func (d *dailyFile) cleanup(now time.Time) {
	cutoff := now.AddDate(0, 0, -logRetentionDays).Format(dateLayout)
	entries, _ := os.ReadDir(d.dir)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		if date := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".log"); date < cutoff {
			_ = os.Remove(filepath.Join(d.dir, name))
		}
	}
}

func (d *dailyFile) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}

// LatestLogFile 回傳 log 目錄中最新 log 檔的絕對路徑。
func LatestLogFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var latest string
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, "agent-") && strings.HasSuffix(n, ".log") && n > latest {
			latest = n
		}
	}
	if latest == "" {
		return "", fmt.Errorf("%s 中沒有 log 檔", dir)
	}
	return filepath.Abs(filepath.Join(dir, latest))
}

// PrintLogTail 輸出最新 log 檔的最後 n 行。
func PrintLogTail(dir string, n int, w io.Writer) error {
	path, err := LatestLogFile(dir)
	if err != nil {
		return err
	}
	lines, err := lastLines(path, n)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "==> %s（最後 %d 行）\n", path, len(lines))
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
	return nil
}

func lastLines(path string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const chunk = 64 * 1024
	var buf []byte
	pos := st.Size()
	for pos > 0 && strings.Count(string(buf), "\n") <= n {
		start := max(pos-chunk, 0)
		b := make([]byte, pos-start)
		if _, err := f.ReadAt(b, start); err != nil {
			return nil, err
		}
		buf = append(b, buf...)
		pos = start
	}
	text := strings.TrimRight(string(buf), "\r\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	if pos > 0 {
		lines = lines[1:] // 第一段可能是不完整的行
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
