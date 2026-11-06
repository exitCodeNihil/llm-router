package usage

import (
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Window is the range of timestamps usage_events has partitions for: from the
// first day of last month up to the first day of the month after next.
// ensurePartitions creates exactly these months, so a timestamp inside the
// window always has a partition.
func Window(now time.Time) (lo, hi time.Time) {
	now = now.UTC()
	cur := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return cur.AddDate(0, -1, 0), cur.AddDate(0, 2, 0)
}

// maxFuture is how far ahead of the control plane's clock an event may be
// stamped before it is treated as clock skew.
const maxFuture = 2 * time.Minute

// ClampTS moves a timestamp the control plane cannot store, or should not
// trust, into the partition window: a zero or future time becomes now, and
// one older than the window becomes its start, so the spend still counts. The
// bool reports whether it changed anything.
func ClampTS(ts, now time.Time) (time.Time, bool) {
	now = now.UTC()
	if ts.IsZero() || ts.After(now.Add(maxFuture)) {
		return now, true
	}
	if lo, _ := Window(now); ts.Before(lo) {
		return lo, true
	}
	return ts.UTC(), false
}

// Sanitize makes an event reported by a gateway storable: ids that are not
// uuids are dropped, text is cut to the lengths the console shows and counts
// that cannot be negative are zeroed. It reports how many fields it changed.
func Sanitize(e *Event) int {
	fixed := 0
	for _, id := range []*string{&e.APIKeyID, &e.UserID, &e.TeamID, &e.DeploymentID, &e.ProviderID} {
		if *id != "" {
			if _, err := uuid.Parse(*id); err != nil {
				*id = ""
				fixed++
			}
		}
	}
	for _, f := range []struct {
		s *string
		n int
	}{{&e.RequestID, 64}, {&e.ModelName, 256}, {&e.ErrorCode, 64}} {
		if len(*f.s) > f.n {
			*f.s = cut(*f.s, f.n)
			fixed++
		}
	}
	if e.RequestID == "" {
		e.RequestID = "unknown"
		fixed++
	}
	if e.ModelName == "" {
		e.ModelName = "unknown"
		fixed++
	}
	for _, n := range []*int{&e.PromptTokens, &e.CompletionTokens, &e.CachedTokens, &e.CacheWrite5m,
		&e.CacheWrite1h, &e.LatencyMS, &e.Attempts, &e.TTFTMs} {
		if *n < 0 {
			*n = 0
			fixed++
		}
	}
	if e.StatusCode < 0 || e.StatusCode > 999 {
		e.StatusCode = 0
		fixed++
	}
	if e.CostUSD < 0 {
		e.CostUSD = 0
		fixed++
	}
	return fixed
}

// cut shortens s to at most n bytes without splitting a UTF-8 sequence.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
