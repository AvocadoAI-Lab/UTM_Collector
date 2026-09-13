package agent

import (
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"strings"
)

const redactedMark = "[REDACTED]"

var bearerRe = regexp.MustCompile(`(?i)(Bearer\s+)[^\s"\\]+`)

// Redactor 移除字串中的機密（token、proxy 密碼）。所有 log 輸出都經過它。
type Redactor struct {
	secrets []string
}

func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		r.secrets = append(r.secrets, s)
		// log 為 JSON 格式，含引號或反斜線的機密會以跳脫後的形式出現
		if b, err := json.Marshal(s); err == nil {
			if esc := string(b[1 : len(b)-1]); esc != s {
				r.secrets = append(r.secrets, esc)
			}
		}
	}
	return r
}

func (r *Redactor) String(s string) string {
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, redactedMark)
	}
	return bearerRe.ReplaceAllString(s, "${1}"+redactedMark)
}

// redactWriter 在寫出前遮蔽機密，包在 log handler 的輸出端，確保任何欄位都不會漏網。
type redactWriter struct {
	w io.Writer
	r *Redactor
}

func (rw *redactWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(rw.w, rw.r.String(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// redactURL 遮蔽 URL 中的密碼，供顯示 proxy 位址使用。
func redactURL(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return "[無法解析的 URL]"
	}
	return u.Redacted()
}

func urlPassword(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return ""
	}
	p, _ := u.User.Password()
	return p
}
