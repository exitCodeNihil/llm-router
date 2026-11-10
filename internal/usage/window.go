package usage

import (
	"math"
	"strings"
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
// one older than the window becomes its second day, so the spend still
// counts. The bool reports whether it changed anything.
func ClampTS(ts, now time.Time) (time.Time, bool) {
	now = now.UTC()
	if ts.IsZero() || ts.After(now.Add(maxFuture)) {
		return now, true
	}
	// A day into the window, not its first instant: partition bounds are
	// dates in the database's time zone, which may not be UTC.
	if lo, _ := Window(now); ts.Before(lo) {
		return lo.Add(24 * time.Hour), true
	}
	return ts.UTC(), false
}

// Sanitize makes an event reported by a gateway storable: ids that are not
// uuids are dropped, text loses NUL bytes and invalid UTF-8 and is cut to the
// lengths the console shows, and counts and costs are brought into the
// columns' ranges. It reports how many fields it changed.
func Sanitize(e *Event) int {
	fixed := 0
	for _, id := range []*string{&e.APIKeyID, &e.UserID, &e.TeamID, &e.DeploymentID, &e.ProviderID} {
		if *id != "" && !isUUID(*id) {
			*id = ""
			fixed++
		}
	}
	for _, f := range []struct {
		s *string
		n int
	}{{&e.RequestID, 64}, {&e.ModelName, 256}, {&e.ErrorCode, 64}, {&e.InferenceGeo, 32}, {&e.Speed, 32}, {&e.Operation, 32}} {
		v := *f.s
		if strings.ContainsRune(v, 0) || !utf8.ValidString(v) {
			v = strings.ToValidUTF8(strings.ReplaceAll(v, "\x00", ""), "")
		}
		if len(v) > f.n {
			v = cut(v, f.n)
		}
		if v != *f.s {
			*f.s = v
			fixed++
		}
	}
	if e.TraceID != "" && !ValidTraceID(e.TraceID) {
		e.TraceID = ""
		fixed++
	}
	if e.ParentSpanID != "" && !ValidSpanID(e.ParentSpanID) {
		e.ParentSpanID = ""
		fixed++
	}
	if e.CostBasis < 0 || e.CostBasis > math.MaxInt16 {
		e.CostBasis = 0
		fixed++
	}
	if e.RequestID == "" {
		e.RequestID = "unknown"
		fixed++
	}
	if e.ModelName == "" {
		e.ModelName = "unknown"
		fixed++
	}
	// The columns are int4: a larger count fails before it reaches Postgres.
	for _, n := range []*int{&e.PromptTokens, &e.CompletionTokens, &e.CachedTokens, &e.CacheWrite5m,
		&e.CacheWrite1h, &e.LatencyMS, &e.Attempts, &e.TTFTMs} {
		switch {
		case *n < 0:
			*n = 0
			fixed++
		case *n > math.MaxInt32:
			*n = math.MaxInt32
			fixed++
		}
	}
	if e.StatusCode < 0 || e.StatusCode > 999 {
		e.StatusCode = 0
		fixed++
	}
	// notional_cost_usd is numeric(18,10): at most 1e8. Nothing real costs
	// that much in one request.
	for _, f := range []*float64{&e.CostUSD, &e.NotionalCostUSD} {
		if *f < 0 || *f >= maxUSD || math.IsNaN(*f) {
			*f = 0
			fixed++
		}
	}
	return fixed
}

const maxUSD = 1e7

// isUUID accepts only the canonical 36-character form Postgres stores.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	_, err := uuid.Parse(s)
	return err == nil
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
