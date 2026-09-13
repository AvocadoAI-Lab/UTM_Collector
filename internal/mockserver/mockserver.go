// Package mockserver 實作《事件接收端 API 規格書》的 mock，並支援故障注入與統計查詢，
// 供 agent 開發與整合測試（Agent 規格書第 11.2 節）使用。
package mockserver

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBodyBytes = 64 << 20

var (
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	siteIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)
	semverRe = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
)

// Fault 描述要注入的故障。各欄位可組合，例如延遲後再回 500。
type Fault struct {
	Status     int      `json:"status,omitempty"`      // 非 0：直接回此狀態碼（500 / 400 / 429 …）
	RetryAfter string   `json:"retry_after,omitempty"` // 搭配 429 的 Retry-After 標頭
	DelayMS    int      `json:"delay_ms,omitempty"`    // 讀完 request 後延遲（毫秒）再處理
	Drop       bool     `json:"drop,omitempty"`        // 不回應，直接斷線
	Count      int      `json:"count,omitempty"`       // >0：套用 N 次後自動解除；0：持續套用
	Paths      []string `json:"paths,omitempty"`       // 套用的路徑，預設 ["/events"]
}

func (f Fault) active() bool { return f.Status != 0 || f.DelayMS > 0 || f.Drop }

type Stats struct {
	Requests          int             `json:"requests"`
	PathCounts        map[string]int  `json:"path_counts"`
	StatusCounts      map[string]int  `json:"status_counts"`
	Drops             int             `json:"drops"`
	BatchesReceived   int             `json:"batches_received"`
	BatchesDuplicated int             `json:"batches_duplicated"`
	EventsReceived    int             `json:"events_received"`
	EventsAccepted    int             `json:"events_accepted"`
	EventsDuplicated  int             `json:"events_duplicated"`
	Heartbeats        int             `json:"heartbeats"`
	LastHeartbeat     json.RawMessage `json:"last_heartbeat"`
}

// AcceptedEvent 為去重後被接受的事件，依接受順序排列。
type AcceptedEvent struct {
	Key        string `json:"key"`
	EventID    string `json:"event_id,omitempty"`
	ReceivedAt string `json:"received_at"`
}

type Server struct {
	token string

	mu         sync.Mutex
	minVersion string
	fault      Fault
	stats      Stats
	batches    map[string]bool
	keys       map[string]bool
	accepted   []AcceptedEvent
}

func New(token string) *Server {
	s := &Server{token: token, minVersion: "1.0.0"}
	s.Reset()
	return s
}

func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = Fault{}
	s.stats = Stats{PathCounts: map[string]int{}, StatusCounts: map[string]int{}}
	s.batches = map[string]bool{}
	s.keys = map[string]bool{}
	s.accepted = nil
}

func (s *Server) SetFault(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = f
}

func (s *Server) ClearFault() { s.SetFault(Fault{}) }

func (s *Server) SetMinSupportedVersion(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.minVersion = v
}

func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.PathCounts = maps.Clone(s.stats.PathCounts)
	st.StatusCounts = maps.Clone(s.stats.StatusCounts)
	return st
}

func (s *Server) Accepted() []AcceptedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.accepted)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_control/") {
		s.control(w, r)
		return
	}
	s.mu.Lock()
	s.stats.Requests++
	s.stats.PathCounts[r.URL.Path]++
	s.mu.Unlock()
	rec := &statusRecorder{ResponseWriter: w}
	defer func() {
		if rec.status != 0 {
			s.mu.Lock()
			s.stats.StatusCounts[strconv.Itoa(rec.status)]++
			s.mu.Unlock()
		}
	}()

	if r.Header.Get("Authorization") != "Bearer "+s.token {
		writeError(rec, http.StatusUnauthorized, "unauthorized", "token 無效")
		return
	}
	body, err := readBody(r)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			writeError(rec, http.StatusRequestEntityTooLarge, "batch_too_large", "request body 過大")
		} else {
			writeError(rec, http.StatusBadRequest, "invalid_body", err.Error())
		}
		return
	}

	if f, ok := s.takeFault(r.URL.Path); ok {
		if f.DelayMS > 0 {
			time.Sleep(time.Duration(f.DelayMS) * time.Millisecond)
		}
		if f.Drop {
			s.mu.Lock()
			s.stats.Drops++
			s.mu.Unlock()
			panic(http.ErrAbortHandler) // 中斷連線，不送出任何回應
		}
		if f.Status != 0 {
			if f.RetryAfter != "" {
				rec.Header().Set("Retry-After", f.RetryAfter)
			}
			writeError(rec, f.Status, "injected_fault", "故障注入")
			return
		}
	}

	switch {
	case r.URL.Path == "/events" && r.Method == http.MethodPost:
		s.handleEvents(rec, body)
	case r.URL.Path == "/heartbeat" && r.Method == http.MethodPost:
		s.handleHeartbeat(rec, body)
	case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
		writeJSON(rec, http.StatusOK, map[string]any{"ok": true, "server_time": time.Now().UTC().Format("2006-01-02T15:04:05.000Z")})
	default:
		writeError(rec, http.StatusNotFound, "not_found", "找不到路徑")
	}
}

func (s *Server) takeFault(path string) (Fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.fault
	if !f.active() {
		return f, false
	}
	paths := f.Paths
	if len(paths) == 0 {
		paths = []string{"/events"}
	}
	if !slices.Contains(paths, path) {
		return f, false
	}
	if s.fault.Count > 0 {
		if s.fault.Count--; s.fault.Count == 0 {
			s.fault = Fault{}
		}
	}
	return f, true
}

type eventIn struct {
	ReceivedAt string `json:"received_at"`
	Raw        string `json:"raw"`
	Parsed     *struct {
		Payload json.RawMessage `json:"payload"`
	} `json:"parsed"`
	ParseError *string `json:"parse_error"`
}

func (s *Server) handleEvents(w http.ResponseWriter, body []byte) {
	var req struct {
		AgentID      string    `json:"agent_id"`
		SiteID       string    `json:"site_id"`
		AgentVersion string    `json:"agent_version"`
		BatchID      string    `json:"batch_id"`
		SentAt       string    `json:"sent_at"`
		Events       []eventIn `json:"events"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if msg := validateBatch(req.AgentID, req.SiteID, req.AgentVersion, req.BatchID, req.SentAt, req.Events); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_batch", msg)
		return
	}
	if len(req.Events) > 10000 {
		writeError(w, http.StatusRequestEntityTooLarge, "batch_too_large", "events 不可超過 10000 筆")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(req.Events)
	s.stats.BatchesReceived++
	s.stats.EventsReceived += n
	if s.batches[req.BatchID] {
		s.stats.BatchesDuplicated++
		s.stats.EventsDuplicated += n
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "received": n, "accepted": 0, "duplicated": n})
		return
	}
	s.batches[req.BatchID] = true
	accepted := 0
	for _, ev := range req.Events {
		key, eventID := eventKey(req.AgentID, ev)
		if s.keys[key] {
			continue
		}
		s.keys[key] = true
		s.accepted = append(s.accepted, AcceptedEvent{Key: key, EventID: eventID, ReceivedAt: ev.ReceivedAt})
		accepted++
	}
	s.stats.EventsAccepted += accepted
	s.stats.EventsDuplicated += n - accepted
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "received": n, "accepted": accepted, "duplicated": n - accepted})
}

func validateBatch(agentID, siteID, version, batchID, sentAt string, events []eventIn) string {
	switch {
	case !uuidRe.MatchString(agentID):
		return "agent_id 必須為 UUID"
	case !siteIDRe.MatchString(siteID):
		return "site_id 格式錯誤"
	case !semverRe.MatchString(version):
		return "agent_version 必須為 semver"
	case batchID == "":
		return "batch_id 不可為空"
	case !strings.HasSuffix(sentAt, "Z"):
		return "sent_at 必須為 UTC 的 RFC 3339 時間"
	case len(events) == 0:
		return "events 欄位不可為空"
	}
	if _, err := time.Parse(time.RFC3339Nano, sentAt); err != nil {
		return "sent_at 必須為 RFC 3339 時間"
	}
	for i, ev := range events {
		if ev.ReceivedAt == "" || ev.Raw == "" {
			return "events[" + strconv.Itoa(i) + "] 缺少 received_at 或 raw"
		}
	}
	return ""
}

// eventKey 依規格 2.3 去重：有 event_id 時用 (agent_id, event_id)，否則用 (agent_id, received_at, raw 的 hash)。
func eventKey(agentID string, ev eventIn) (key, eventID string) {
	if ev.ParseError == nil && ev.Parsed != nil {
		var p struct {
			EventID string `json:"event_id"`
		}
		if json.Unmarshal(ev.Parsed.Payload, &p) == nil && p.EventID != "" {
			return agentID + "|event_id|" + p.EventID, p.EventID
		}
	}
	sum := sha256.Sum256([]byte(ev.Raw))
	return agentID + "|" + ev.ReceivedAt + "|" + hex.EncodeToString(sum[:]), ""
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, body []byte) {
	var hb struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(body, &hb); err != nil || !uuidRe.MatchString(hb.AgentID) {
		writeError(w, http.StatusBadRequest, "invalid_heartbeat", "agent_id 必須為 UUID")
		return
	}
	s.mu.Lock()
	s.stats.Heartbeats++
	s.stats.LastHeartbeat = json.RawMessage(body)
	minVersion := s.minVersion
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "min_supported_version": minVersion})
}

// control 為測試用的控制端點（無需認證，僅存在於 mock）。
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/_control/stats" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.Stats())
	case r.URL.Path == "/_control/events" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.Accepted())
	case r.URL.Path == "/_control/fault" && r.Method == http.MethodPost:
		var f Fault
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_fault", err.Error())
			return
		}
		s.SetFault(f)
		writeJSON(w, http.StatusOK, f)
	case r.URL.Path == "/_control/fault" && r.Method == http.MethodDelete:
		s.ClearFault()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.URL.Path == "/_control/min-version" && r.Method == http.MethodPost:
		var v struct {
			MinSupportedVersion string `json:"min_supported_version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
		s.SetMinSupportedVersion(v.MinSupportedVersion)
		writeJSON(w, http.StatusOK, v)
	case r.URL.Path == "/_control/reset" && r.Method == http.MethodPost:
		s.Reset()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeError(w, http.StatusNotFound, "not_found", "找不到控制端點")
	}
}

var errTooLarge = errors.New("body too large")

func readBody(r *http.Request) ([]byte, error) {
	var rd io.Reader = io.LimitReader(r.Body, maxBodyBytes+1)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(rd)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		rd = io.LimitReader(zr, maxBodyBytes+1)
	}
	body, err := io.ReadAll(rd)
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
		return nil, errTooLarge
	}
	return body, nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": map[string]string{"code": code, "message": msg}})
}
