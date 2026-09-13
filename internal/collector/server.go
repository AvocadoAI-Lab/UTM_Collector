package collector

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"pico-utm-agent/internal/protocol"
)

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	sitePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,63}$`)
	semverPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	errTooLarge   = errors.New("request body too large")
)

type Server struct {
	store        Store
	tokenHashes  [][32]byte
	minVersion   string
	maxBodyBytes int64
	log          *slog.Logger
	now          func() time.Time
}

func NewServer(store Store, cfg Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{store: store, minVersion: cfg.MinSupportedVersion, maxBodyBytes: cfg.MaxBodyBytes, log: log, now: time.Now}
	for _, token := range cfg.Tokens {
		s.tokenHashes = append(s.tokenHashes, sha256.Sum256([]byte(token)))
	}
	return s
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !s.authorized(r.Header.Get("Authorization")) {
		writeError(w, 401, "unauthorized", "invalid bearer token")
		return
	}
	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.store.Ping(ctx); err != nil {
			s.log.Error("health check failed", "error", err)
			writeError(w, 503, "unavailable", "database unavailable")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "server_time": s.now().UTC().Format("2006-01-02T15:04:05.000Z")})
	case "/events":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleEvents(w, r)
	case "/heartbeat":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleHeartbeat(w, r)
	default:
		writeError(w, 404, "not_found", "endpoint not found")
	}
}

func (s *Server) authorized(value string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	h := sha256.Sum256([]byte(strings.TrimPrefix(value, prefix)))
	ok := 0
	for _, want := range s.tokenHashes {
		ok |= subtle.ConstantTimeCompare(h[:], want[:])
	}
	return ok == 1
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	body, err := s.readBody(w, r)
	if err != nil {
		bodyError(w, err)
		return
	}
	var req protocol.EventsRequest
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, 400, "invalid_json", err.Error())
		return
	}
	if msg := validateEvents(req); msg != "" {
		writeError(w, 400, "invalid_batch", msg)
		return
	}
	if len(req.Events) > 10000 {
		writeError(w, 413, "batch_too_large", "events must not exceed 10000 items")
		return
	}
	accepted, duplicated, err := s.store.SaveEvents(r.Context(), req)
	if err != nil {
		s.log.Error("save events failed", "agent_id", req.AgentID, "batch_id", req.BatchID, "error", err)
		writeError(w, 503, "unavailable", "storage unavailable")
		return
	}
	writeJSON(w, 200, protocol.EventsResponse{OK: true, Received: len(req.Events), Accepted: accepted, Duplicated: duplicated})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	body, err := s.readBody(w, r)
	if err != nil {
		bodyError(w, err)
		return
	}
	var req protocol.HeartbeatRequest
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, 400, "invalid_json", err.Error())
		return
	}
	if msg := validateHeartbeat(req); msg != "" {
		writeError(w, 400, "invalid_heartbeat", msg)
		return
	}
	if err := s.store.SaveHeartbeat(r.Context(), req, body); err != nil {
		s.log.Error("save heartbeat failed", "agent_id", req.AgentID, "error", err)
		writeError(w, 503, "unavailable", "storage unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "min_supported_version": s.minVersion})
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if ct := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]); ct != "application/json" {
		return nil, fmt.Errorf("content-type must be application/json")
	}
	compressed := http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	var reader io.Reader = compressed
	switch enc := strings.TrimSpace(strings.ToLower(r.Header.Get("Content-Encoding"))); enc {
	case "":
	case "gzip":
		gz, err := gzip.NewReader(compressed)
		if err != nil {
			return nil, fmt.Errorf("invalid gzip body: %w", err)
		}
		defer gz.Close()
		reader = gz
	default:
		return nil, fmt.Errorf("unsupported content-encoding %q", enc)
	}
	b, err := io.ReadAll(io.LimitReader(reader, s.maxBodyBytes+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, errTooLarge
		}
		return nil, err
	}
	if int64(len(b)) > s.maxBodyBytes {
		return nil, errTooLarge
	}
	return b, nil
}

func decodeStrict(b []byte, dst any) error {
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateCommon(agentID, siteID, version, sentAt string) string {
	if !uuidPattern.MatchString(agentID) {
		return "agent_id must be a UUID"
	}
	if !sitePattern.MatchString(siteID) {
		return "site_id has an invalid format"
	}
	if !semverPattern.MatchString(version) {
		return "agent_version must be semantic versioning"
	}
	if !strings.HasSuffix(sentAt, "Z") {
		return "sent_at must be an RFC 3339 UTC timestamp"
	}
	if _, err := time.Parse(time.RFC3339Nano, sentAt); err != nil {
		return "sent_at must be an RFC 3339 UTC timestamp"
	}
	return ""
}
func validateEvents(r protocol.EventsRequest) string {
	if msg := validateCommon(r.AgentID, r.SiteID, r.AgentVersion, r.SentAt); msg != "" {
		return msg
	}
	if strings.TrimSpace(r.BatchID) == "" {
		return "batch_id is required"
	}
	if len(r.Events) == 0 {
		return "events must not be empty"
	}
	for i, e := range r.Events {
		if _, err := time.Parse(time.RFC3339Nano, e.ReceivedAt); err != nil {
			return fmt.Sprintf("events[%d].received_at must be RFC 3339", i)
		}
		if e.Raw == "" {
			return fmt.Sprintf("events[%d].raw is required", i)
		}
		if e.SourceIP != "" && net.ParseIP(e.SourceIP) == nil {
			return fmt.Sprintf("events[%d].source_ip is invalid", i)
		}
		if len(e.Parsed) > 0 && !json.Valid(e.Parsed) {
			return fmt.Sprintf("events[%d].parsed is invalid", i)
		}
	}
	return ""
}
func validateHeartbeat(r protocol.HeartbeatRequest) string {
	if msg := validateCommon(r.AgentID, r.SiteID, r.AgentVersion, r.SentAt); msg != "" {
		return msg
	}
	if r.Hostname == "" {
		return "hostname is required"
	}
	if r.OS == "" {
		return "os is required"
	}
	if r.LastEventReceivedAt != nil {
		if _, err := time.Parse(time.RFC3339Nano, *r.LastEventReceivedAt); err != nil {
			return "last_event_received_at must be RFC 3339"
		}
	}
	if r.LastForwardSuccessAt != nil {
		if _, err := time.Parse(time.RFC3339Nano, *r.LastForwardSuccessAt); err != nil {
			return "last_forward_success_at must be RFC 3339"
		}
	}
	for _, ip := range r.UTMSourceIPs {
		if net.ParseIP(ip) == nil {
			return "utm_source_ips contains an invalid IP"
		}
	}
	return ""
}
func bodyError(w http.ResponseWriter, err error) {
	if errors.Is(err, errTooLarge) {
		writeError(w, 413, "batch_too_large", "request body too large")
	} else {
		writeError(w, 400, "invalid_body", err.Error())
	}
}
func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, 405, "method_not_allowed", "method not allowed")
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, protocol.ErrorResponse{OK: false, Error: protocol.APIError{Code: code, Message: message}})
}
