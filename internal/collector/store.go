package collector

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"pico-utm-agent/internal/protocol"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store interface {
	SaveEvents(context.Context, protocol.EventsRequest) (accepted int, duplicated int, err error)
	SaveHeartbeat(context.Context, protocol.HeartbeatRequest, []byte) error
	Ping(context.Context) error
}

type PostgresStore struct{ pool *pgxpool.Pool }

func OpenPostgres(ctx context.Context, url string) (*PostgresStore, error) {
	p, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	s := &PostgresStore{pool: p}
	if err := s.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return s, nil
}
func (s *PostgresStore) Close()                         { s.pool.Close() }
func (s *PostgresStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
func (s *PostgresStore) Migrate(ctx context.Context) error {
	b, err := migrations.ReadFile("migrations/001_init.sql")
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, string(b))
	return err
}

func (s *PostgresStore) SaveEvents(ctx context.Context, r protocol.EventsRequest) (int, int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO batches(agent_id,batch_id,site_id,agent_version,sent_at,event_count) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, r.AgentID, r.BatchID, r.SiteID, r.AgentVersion, r.SentAt, len(r.Events))
	if err != nil {
		return 0, 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, len(r.Events), tx.Commit(ctx)
	}
	accepted := 0
	for _, e := range r.Events {
		h := sha256.Sum256([]byte(e.Raw))
		rawHash := hex.EncodeToString(h[:])
		key, eventID := eventKey(r.AgentID, e, rawHash)
		var parsed any
		if len(e.Parsed) > 0 && string(e.Parsed) != "null" {
			parsed = e.Parsed
		}
		tag, err = tx.Exec(ctx, `INSERT INTO events(agent_id,batch_id,site_id,event_key,event_id,event_received_at,source_ip,raw,raw_base64,raw_hash,parsed,parse_error) VALUES($1,$2,$3,$4,NULLIF($5,''),$6,NULLIF($7,'')::inet,$8,NULLIF($9,''),$10,$11,$12) ON CONFLICT(agent_id,event_key) DO NOTHING`, r.AgentID, r.BatchID, r.SiteID, key, eventID, e.ReceivedAt, e.SourceIP, e.Raw, e.RawBase64, rawHash, parsed, e.ParseError)
		if err != nil {
			return 0, 0, err
		}
		accepted += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return accepted, len(r.Events) - accepted, nil
}

func eventKey(agentID string, e protocol.Event, rawHash string) (string, string) {
	if e.ParseError == nil && len(e.Parsed) > 0 {
		var p struct {
			Payload struct {
				EventID any `json:"event_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(e.Parsed, &p) == nil && p.Payload.EventID != nil {
			id := fmt.Sprint(p.Payload.EventID)
			if id != "" {
				return "event_id:" + id, id
			}
		}
	}
	return "fallback:" + e.ReceivedAt + ":" + rawHash, ""
}

func (s *PostgresStore) SaveHeartbeat(ctx context.Context, h protocol.HeartbeatRequest, raw []byte) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO heartbeats(agent_id,site_id,agent_version,sent_at,payload) VALUES($1,$2,$3,$4,$5) ON CONFLICT(agent_id) DO UPDATE SET site_id=excluded.site_id,agent_version=excluded.agent_version,sent_at=excluded.sent_at,received_at=now(),payload=excluded.payload`, h.AgentID, h.SiteID, h.AgentVersion, h.SentAt, raw)
	return err
}
