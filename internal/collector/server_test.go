package collector

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"pico-utm-agent/internal/protocol"
)

type fakeStore struct {
	accepted, duplicated int
	saveErr, pingErr     error
	events               protocol.EventsRequest
	heartbeat            protocol.HeartbeatRequest
}

func (f *fakeStore) SaveEvents(_ context.Context, r protocol.EventsRequest) (int, int, error) {
	f.events = r
	return f.accepted, f.duplicated, f.saveErr
}
func (f *fakeStore) SaveHeartbeat(_ context.Context, h protocol.HeartbeatRequest, _ []byte) error {
	f.heartbeat = h
	return f.saveErr
}
func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

func testServer(f *fakeStore, max int64) *Server {
	s := NewServer(f, Config{Tokens: []string{"secret"}, MinSupportedVersion: "1.2.3", MaxBodyBytes: max}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return time.Date(2026, 9, 11, 8, 35, 0, 0, time.UTC) }
	return s
}
func request(t *testing.T, s *Server, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func validBatch() []byte {
	return []byte(`{"agent_id":"550e8400-e29b-41d4-a716-446655440000","site_id":"acme-taipei-hq","agent_version":"1.0.0","batch_id":"018f3c2a-7b1e-7000-8000-000000000001","sent_at":"2026-09-11T08:33:00.000Z","events":[{"received_at":"2026-09-11T08:32:46.123456Z","source_ip":"192.168.1.198","raw":"<14>event","parsed":{"payload":{"event_id":"7"}},"parse_error":null}]}`)
}
func validHeartbeat() []byte {
	return []byte(`{"agent_id":"550e8400-e29b-41d4-a716-446655440000","site_id":"acme-taipei-hq","agent_version":"1.0.0","hostname":"collector","os":"windows/amd64","sent_at":"2026-09-11T08:35:00Z","uptime_seconds":3,"last_event_received_at":null,"events_received_total":1,"events_forwarded_total":1,"events_dead_lettered_total":0,"events_dropped_total":0,"spool_write_errors_total":0,"spool_backlog_bytes":0,"spool_total_bytes":2,"spool_disk_free_bytes":3,"last_forward_success_at":null,"last_forward_error":null,"proxy_in_use":false,"utm_source_ips":["192.168.1.198"]}`)
}

func TestHealthAndAuthentication(t *testing.T) {
	f := &fakeStore{}
	s := testServer(f, 1024)
	w := request(t, s, "GET", "/healthz", nil, nil)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"server_time":"2026-09-11T08:35:00.000Z"`)) {
		t.Fatalf("response %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", "/healthz", nil)
	x := httptest.NewRecorder()
	s.Handler().ServeHTTP(x, r)
	if x.Code != 401 {
		t.Fatalf("want 401 got %d", x.Code)
	}
	f.pingErr = errors.New("down")
	w = request(t, s, "GET", "/healthz", nil, nil)
	if w.Code != 503 {
		t.Fatalf("want 503 got %d", w.Code)
	}
}
func TestEventsJSONAndGzip(t *testing.T) {
	for _, gzipBody := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "gzip"}[gzipBody], func(t *testing.T) {
			f := &fakeStore{accepted: 1}
			s := testServer(f, 4096)
			body := validBatch()
			headers := map[string]string{"Content-Type": "application/json"}
			if gzipBody {
				var b bytes.Buffer
				z := gzip.NewWriter(&b)
				_, _ = z.Write(body)
				_ = z.Close()
				body = b.Bytes()
				headers["Content-Encoding"] = "gzip"
			}
			w := request(t, s, "POST", "/events", body, headers)
			if w.Code != 200 {
				t.Fatalf("response %d %s", w.Code, w.Body.String())
			}
			var out protocol.EventsResponse
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Accepted != 1 || out.Received != 1 {
				t.Fatalf("response %#v err %v", out, err)
			}
			if len(f.events.Events) != 1 {
				t.Fatal("store did not receive event")
			}
		})
	}
}
func TestEventsValidationAndFailures(t *testing.T) {
	cases := []struct {
		name        string
		body        []byte
		contentType string
		max         int64
		storeErr    error
		want        int
	}{{"content type", validBatch(), "text/plain", 4096, nil, 400}, {"unknown field", append(validBatch()[:len(validBatch())-1], []byte(`,"extra":1}`)...), "application/json", 4096, nil, 400}, {"too large", validBatch(), "application/json", 32, nil, 413}, {"store", validBatch(), "application/json", 4096, errors.New("down"), 503}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeStore{saveErr: tc.storeErr}
			w := request(t, testServer(f, tc.max), "POST", "/events", tc.body, map[string]string{"Content-Type": tc.contentType})
			if w.Code != tc.want {
				t.Fatalf("want %d got %d: %s", tc.want, w.Code, w.Body.String())
			}
		})
	}
}
func TestHeartbeat(t *testing.T) {
	f := &fakeStore{}
	w := request(t, testServer(f, 4096), "POST", "/heartbeat", validHeartbeat(), map[string]string{"Content-Type": "application/json"})
	if w.Code != 200 || f.heartbeat.Hostname != "collector" || !bytes.Contains(w.Body.Bytes(), []byte(`"min_supported_version":"1.2.3"`)) {
		t.Fatalf("response %d %s stored %#v", w.Code, w.Body.String(), f.heartbeat)
	}
}
