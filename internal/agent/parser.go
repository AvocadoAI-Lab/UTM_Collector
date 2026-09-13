package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Record 為 spool 中的一筆記錄（JSONL 的一行），也是 /events 批次中的一筆事件。
type Record struct {
	ReceivedAt string `json:"received_at"`
	SourceIP   string `json:"source_ip"`
	Raw        string `json:"raw"`
	// RawBase64 僅在原始封包含非 UTF-8 位元組時出現，保留完整原始位元組。
	RawBase64  string  `json:"raw_base64,omitempty"`
	Parsed     *Parsed `json:"parsed"`
	ParseError *string `json:"parse_error"`
}

type Parsed struct {
	Program  string `json:"program"`
	Hostname string `json:"hostname"`
	PID      *int   `json:"pid"`
	Priority int    `json:"priority"`
	// Payload 原樣保留 JSON（不套用 schema）；payload 不是 JSON 物件時為 null。
	Payload json.RawMessage `json:"payload"`
}

// ParseSyslog 解析 `<PRI>MMM DD HH:MM:SS HOSTNAME PROGRAM[PID]: PAYLOAD`。
// 標頭無法解析時回傳 nil 與錯誤；標頭正常但 payload 不是 JSON 物件時，回傳解析結果（payload 為 null）與錯誤。
// program 名稱不做任何限制。
func ParseSyslog(msg string) (*Parsed, error) {
	if !strings.HasPrefix(msg, "<") {
		return nil, errors.New("缺少 <PRI>")
	}
	end := strings.IndexByte(msg, '>')
	if end < 2 || end > 4 {
		return nil, errors.New("<PRI> 格式錯誤")
	}
	pri, err := strconv.Atoi(msg[1:end])
	if err != nil || pri < 0 || pri > 191 {
		return nil, errors.New("<PRI> 不是 0–191 的數字")
	}
	rest := msg[end+1:]
	if len(rest) < 16 || rest[15] != ' ' {
		return nil, errors.New("缺少時間戳記")
	}
	if _, err := time.Parse(time.Stamp, rest[:15]); err != nil {
		return nil, errors.New("時間戳記格式錯誤")
	}
	rest = rest[16:]
	sp := strings.IndexByte(rest, ' ')
	if sp <= 0 {
		return nil, errors.New("缺少 hostname")
	}
	p := &Parsed{Hostname: rest[:sp], Priority: pri}
	rest = rest[sp+1:]

	colon := strings.IndexByte(rest, ':')
	if colon <= 0 || strings.ContainsAny(rest[:colon], " \t") {
		return nil, errors.New("缺少 program 標籤")
	}
	tag := rest[:colon]
	if lb := strings.IndexByte(tag, '['); lb > 0 && strings.HasSuffix(tag, "]") {
		pid, err := strconv.Atoi(tag[lb+1 : len(tag)-1])
		if err != nil {
			return nil, errors.New("PID 不是數字")
		}
		p.PID = &pid
		tag = tag[:lb]
	}
	p.Program = tag

	payload := strings.TrimSpace(rest[colon+1:])
	if !strings.HasPrefix(payload, "{") || !json.Valid([]byte(payload)) {
		return p, errors.New("payload 不是 JSON 物件")
	}
	p.Payload = json.RawMessage(payload)
	return p, nil
}

// BuildRecord 將一個 UDP 封包轉為 spool 記錄。解析失敗時仍保留原始內容並標記 parse_error。
func BuildRecord(data []byte, sourceIP string, receivedAt time.Time) Record {
	rec := Record{
		ReceivedAt: formatMicros(receivedAt),
		SourceIP:   sourceIP,
		Raw:        string(data),
	}
	var errs []string
	if !utf8.Valid(data) {
		rec.Raw = strings.ToValidUTF8(rec.Raw, "�")
		rec.RawBase64 = base64.StdEncoding.EncodeToString(data)
		errs = append(errs, "包含非 UTF-8 位元組（原始內容見 raw_base64）")
	}
	parsed, err := ParseSyslog(rec.Raw)
	rec.Parsed = parsed
	if err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		msg := strings.Join(errs, "；")
		rec.ParseError = &msg
	}
	return rec
}

// MarshalRecord 輸出一行 JSON（含結尾換行），不做 HTML 跳脫以保持 raw 可讀。
func MarshalRecord(rec Record) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// eventIDOf 取出 spool 行中的 parsed.payload.event_id，沒有則回傳空字串。僅用於 log。
func eventIDOf(line []byte) string {
	var r struct {
		Parsed *struct {
			Payload struct {
				EventID string `json:"event_id"`
			} `json:"payload"`
		} `json:"parsed"`
	}
	if json.Unmarshal(line, &r) != nil || r.Parsed == nil {
		return ""
	}
	return r.Parsed.Payload.EventID
}
