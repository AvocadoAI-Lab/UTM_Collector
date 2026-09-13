package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

type eventsBatch struct {
	AgentID      string            `json:"agent_id"`
	SiteID       string            `json:"site_id"`
	AgentVersion string            `json:"agent_version"`
	BatchID      string            `json:"batch_id"`
	SentAt       string            `json:"sent_at"`
	Events       []json.RawMessage `json:"events"`
}

type deadLetter struct {
	DeadLetteredAt string      `json:"dead_lettered_at"`
	Status         int         `json:"status"`
	ResponseBody   string      `json:"response_body"`
	Batch          eventsBatch `json:"batch"`
}

// Forwarder 從 spool 讀取批次並送到 /events；只有 2xx（或 4xx 進 dead-letter 後）才推進 offset。
type Forwarder struct {
	cfg     *Config
	spool   *Spool
	api     *apiClient
	stats   *Stats
	clock   Clock
	log     *slog.Logger
	backoff Backoff

	pending atomic.Int64 // 啟動後寫入 spool、尚未送出的筆數
	wake    chan struct{}
}

// Notify 由 receiver 在每筆寫入 spool 後呼叫；累積達 batch_size 時喚醒 forwarder。
func (f *Forwarder) Notify() {
	if f.pending.Add(1) >= int64(f.cfg.Forwarder.BatchSize) {
		select {
		case f.wake <- struct{}{}:
		default:
		}
	}
}

func (f *Forwarder) Run(ctx context.Context) {
	batchSize := f.cfg.Forwarder.BatchSize
	interval := f.cfg.batchInterval()
	lastSend := f.clock.Now()
	startup := true // 啟動時立即送出上次遺留的積壓
	skipWait := true
	for {
		if !skipWait {
			if wait := interval - f.clock.Now().Sub(lastSend); wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-f.wake:
				case <-f.clock.After(wait):
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		skipWait = false

		intervalDue := f.clock.Now().Sub(lastSend) >= interval
		lines, end, err := f.spool.ReadReadyBatch(batchSize, intervalDue || startup)
		if err != nil {
			f.log.Error("讀取 spool 失敗", "error", err)
			lastSend = f.clock.Now()
			continue
		}
		if len(lines) == 0 {
			if intervalDue {
				lastSend = f.clock.Now()
			}
			startup = false
			continue
		}
		startup = false

		if !f.deliver(ctx, lines, end) {
			return
		}
		if f.pending.Add(-int64(len(lines))) < 0 {
			f.pending.Store(0)
		}
		lastSend = f.clock.Now()
		skipWait = len(lines) == batchSize // 批次滿載代表可能還有積壓，立刻再讀
	}
}

// deliver 持續嘗試送出同一批次（相同 batch_id），直到成功或進 dead-letter；ctx 結束時回傳 false。
func (f *Forwarder) deliver(ctx context.Context, lines []json.RawMessage, end Position) bool {
	batch := eventsBatch{
		AgentID:      f.cfg.Agent.AgentID,
		SiteID:       f.cfg.Agent.SiteID,
		AgentVersion: Version,
		BatchID:      newUUIDv7(f.clock.Now()),
		Events:       lines,
	}
	logAttrs := []any{
		"batch_id", batch.BatchID, "events", len(lines),
		"first_event_id", eventIDOf(lines[0]), "last_event_id", eventIDOf(lines[len(lines)-1]),
	}

	for attempt := 1; ; attempt++ {
		batch.SentAt = formatMillis(f.clock.Now())
		start := time.Now()
		res, err := f.api.post(ctx, "/events", batch)
		elapsed := time.Since(start).Milliseconds()
		if ctx.Err() != nil {
			return false
		}

		var delay time.Duration
		switch {
		case err != nil:
			f.failed(fmt.Sprintf("連線失敗：%v", err))
			delay = f.backoff.Next()
			f.log.Warn("批次轉送失敗（連線），退避後重試", append(logAttrs, "attempt", attempt, "duration_ms", elapsed, "error", err, "retry_in", delay.String())...)

		case res.status >= 200 && res.status < 300:
			f.commit(end)
			f.backoff.Reset()
			now := formatMillis(f.clock.Now())
			f.stats.update(func(v *statsValues) {
				v.forwarded += int64(len(lines))
				v.lastForwardSuccessAt = now
				v.lastForwardError = ""
			})
			f.log.Info("批次轉送成功", append(logAttrs, "attempt", attempt, "status", res.status, "duration_ms", elapsed, "response", summarizeBody(res.body))...)
			return true

		case res.status == 429:
			f.failed("HTTP 429")
			d, ok := parseRetryAfter(res.header.Get("Retry-After"), f.clock.Now())
			if !ok {
				d = f.backoff.Next()
			}
			delay = d
			f.log.Warn("接收端速率限制（429），退避後重試", append(logAttrs, "attempt", attempt, "duration_ms", elapsed, "retry_after", res.header.Get("Retry-After"), "retry_in", delay.String())...)

		case res.status >= 400 && res.status < 500:
			if err := f.writeDeadLetter(batch, res); err != nil {
				// dead-letter 寫不進去就不推進 offset，避免事件遺失
				delay = f.backoff.Next()
				f.log.Error("寫入 dead-letter 失敗，暫不推進 offset", append(logAttrs, "status", res.status, "error", err, "retry_in", delay.String())...)
				break
			}
			f.commit(end)
			f.backoff.Reset()
			f.stats.update(func(v *statsValues) {
				v.deadLettered += int64(len(lines))
				v.lastForwardError = fmt.Sprintf("HTTP %d（批次已寫入 dead-letter）", res.status)
			})
			f.log.Error("接收端拒絕批次，已寫入 dead-letter 並推進 offset", append(logAttrs, "attempt", attempt, "status", res.status, "duration_ms", elapsed, "response", summarizeBody(res.body))...)
			return true

		default: // 5xx 與其他非預期狀態碼一律重試
			f.failed(fmt.Sprintf("HTTP %d", res.status))
			delay = f.backoff.Next()
			f.log.Warn("批次轉送失敗，退避後重試", append(logAttrs, "attempt", attempt, "status", res.status, "duration_ms", elapsed, "retry_in", delay.String())...)
		}

		select {
		case <-ctx.Done():
			return false
		case <-f.clock.After(delay):
		}
	}
}

func (f *Forwarder) commit(end Position) {
	if err := f.spool.Commit(end); err != nil {
		f.log.Error("寫入 offset 檔失敗（重啟後可能重送）", "error", err)
	}
}

func (f *Forwarder) failed(msg string) {
	f.stats.update(func(v *statsValues) { v.lastForwardError = msg })
}

func (f *Forwarder) writeDeadLetter(batch eventsBatch, res *apiResponse) error {
	body := res.body
	if len(body) > 1024 {
		body = body[:1024]
	}
	line, err := json.Marshal(deadLetter{
		DeadLetteredAt: formatMillis(f.clock.Now()),
		Status:         res.status,
		ResponseBody:   strings.ToValidUTF8(string(body), "�"),
		Batch:          batch,
	})
	if err != nil {
		return err
	}
	return f.spool.WriteDeadLetter(f.clock.Now(), append(line, '\n'))
}

// summarizeBody 將回應 body 截短供 log 使用。
func summarizeBody(b []byte) string {
	s := strings.ToValidUTF8(strings.TrimSpace(string(b)), "�")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
