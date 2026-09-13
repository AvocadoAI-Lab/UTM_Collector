package agent

import (
	"strconv"
	"strings"
	"time"
)

// Version 於建置時以 -ldflags "-X pico-utm-agent/internal/agent.Version=x.y.z" 覆寫。
var Version = "1.0.0"

// versionLess 比較 semver 的 major.minor.patch，忽略 pre-release 與 build metadata。
func versionLess(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func versionParts(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, s := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(s)
	}
	return out
}

func formatMicros(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z") }
func formatMillis(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
