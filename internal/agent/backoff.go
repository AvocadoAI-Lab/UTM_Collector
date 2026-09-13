package agent

import (
	"crypto/rand"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var backoffSteps = []time.Duration{1 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}

const maxRetryAfter = time.Hour

// Backoff 產生 1s → 5s → 15s → 30s → 60s（之後維持 60s）的退避時間，並加入 ±20% jitter。
type Backoff struct {
	attempt int
	rand    func() float64 // 回傳 [0,1)；nil 時使用 math/rand
}

func (b *Backoff) Next() time.Duration {
	i := min(b.attempt, len(backoffSteps)-1)
	b.attempt++
	r := mrand.Float64()
	if b.rand != nil {
		r = b.rand()
	}
	return jitter(backoffSteps[i], r)
}

func (b *Backoff) Reset() { b.attempt = 0 }

// jitter 將 d 乘上 [0.8, 1.2) 之間的係數。
func jitter(d time.Duration, r float64) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*r))
}

// parseRetryAfter 解析 Retry-After（秒數或 HTTP 日期），上限一小時。
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return min(time.Duration(secs)*time.Second, maxRetryAfter), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(t.Sub(now), 0), maxRetryAfter), true
	}
	return 0, false
}

// newUUIDv7 產生時間排序的 UUID v7，作為 batch_id。
func newUUIDv7(now time.Time) string {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	_, _ = rand.Read(b[6:])
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
