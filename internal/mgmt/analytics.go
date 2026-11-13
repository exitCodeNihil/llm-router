package mgmt

import (
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// One filter definition shared by the request list and every aggregate, so a
// number on the analytics page always describes the same rows the drill-down
// shows. Previously the aggregates accepted only ?days and silently ignored
// every dimension, which is why the dashboard could not answer "spend for this
// team on this model".
//
// Conditions are written against the alias `e` (usage_events) and `k`
// (api_keys), so every consumer must use the same aliases.
type usageFilter struct {
	conds []string
	args  []any
	// NeedsKeys is set when a condition references the api_keys join, so callers
	// can skip it when nothing asked for it.
	NeedsKeys bool
	// Err is set when a filter value is malformed (a non-uuid id); handlers
	// answer 400 instead of letting the driver fail the query.
	Err error
	// source is the FROM item of an aggregate query: the rollups for the time
	// range (usageSource). Empty for the raw request feed.
	source string
}

// uuidParams are the filters that compare against uuid columns; anything
// else there makes the driver fail the query. request_id is text (req_…).
var uuidParams = map[string]bool{"key_id": true, "user_id": true, "team_id": true, "provider_id": true, "deployment_id": true, "gateway_id": true}

// maxUsageSpan is the longest time range one usage query may cover.
const maxUsageSpan = 366 * 24 * time.Hour

// newUsageFilter reads the query string and the caller's visibility scope.
// aggregate filters the rollups, for totals and breakdowns; otherwise it
// filters raw events, for the request feed.
func newUsageFilter(r *http.Request, c *Caller, aggregate bool) *usageFilter {
	q := r.URL.Query()
	f := &usageFilter{}

	if !c.IsAdmin {
		f.args = append(f.args, c.UserID)
		n := "$" + strconv.Itoa(len(f.args))
		f.conds = append(f.conds,
			`(e.user_id = `+n+` OR e.team_id = ANY(SELECT team_id FROM team_members WHERE user_id = `+n+`))`)
	}

	add := func(cond string, val any) {
		f.args = append(f.args, val)
		f.conds = append(f.conds, strings.Replace(cond, "$?", "$"+strconv.Itoa(len(f.args)), 1))
	}

	for param, col := range map[string]string{
		"key_id":        "e.api_key_id",
		"user_id":       "e.user_id",
		"team_id":       "e.team_id",
		"model":         "e.model_name",
		"provider_id":   "e.provider_id",
		"deployment_id": "e.deployment_id",
		"error_code":    "e.error_code",
		"gateway_id":    "e.gateway_id",
		"request_id":    "e.request_id",
	} {
		if v := q.Get(param); v != "" {
			if aggregate && param == "request_id" {
				continue // the source reads that request's events
			}
			if uuidParams[param] {
				if _, err := uuid.Parse(v); err != nil {
					f.Err = fmt.Errorf("%s: not a valid id", param)
					continue
				}
			}
			add(col+` = $?`, v)
		}
	}

	switch q.Get("status") {
	case "ok":
		f.conds = append(f.conds, `e.status_code BETWEEN 200 AND 299`)
	case "err":
		f.conds = append(f.conds, `e.status_code >= 400`)
	}
	switch q.Get("priced") {
	case "priced":
		f.conds = append(f.conds, `NOT e.unpriced`)
	case "unpriced":
		f.conds = append(f.conds, `e.unpriced`)
	}
	switch q.Get("stream") {
	case "true":
		f.conds = append(f.conds, `e.stream`)
	case "false":
		f.conds = append(f.conds, `NOT e.stream`)
	}
	if q.Get("failover") == "true" {
		if aggregate {
			f.conds = append(f.conds, `e.failover`)
		} else {
			f.conds = append(f.conds, `e.attempts > 1`)
		}
	}
	if v := q.Get("tag"); v != "" {
		// ponytail: key-level tags only; usage_events stores no tags column
		add(`$? = ANY(k.tags)`, v)
		f.NeedsKeys = true
	}

	// Time window: explicit since/until win, otherwise ?days. There is always
	// a lower bound, so no request can scan all of history.
	parse := func(name string) (time.Time, bool) {
		v := q.Get(name)
		if v == "" {
			return time.Time{}, false
		}
		ts, err := time.Parse(time.RFC3339, v)
		if err != nil {
			f.Err = fmt.Errorf("%s: not a valid time (RFC 3339)", name)
			return time.Time{}, false
		}
		return ts, true
	}
	since, hasSince := parse("since")
	until, hasUntil := parse("until")
	end := time.Now()
	if hasUntil {
		end = until
	}
	if !hasSince {
		since = end.AddDate(0, 0, -daysParam(r))
	}
	if end.Sub(since) > maxUsageSpan {
		f.Err = fmt.Errorf("time range is longer than %d days", int(maxUsageSpan.Hours()/24))
	}
	if aggregate {
		hourly := q.Get("bucket") == "hour" || end.Sub(since) <= 48*time.Hour
		f.source = usageSource(f.next, since, end, hourly, q.Get("request_id"))
		return f
	}
	if hasUntil {
		add(`e.ts <= $?`, until)
	}
	add(`e.ts >= $?`, since)
	return f
}

// where returns the SQL condition, always safe to interpolate after WHERE.
func (f *usageFilter) where() string {
	if len(f.conds) == 0 {
		return "TRUE"
	}
	return strings.Join(f.conds, " AND ")
}

// next reserves another placeholder, for a condition the caller adds itself.
func (f *usageFilter) next(val any) string {
	f.args = append(f.args, val)
	return "$" + strconv.Itoa(len(f.args))
}

// from returns the FROM clause, joining api_keys only when a filter needs it.
func (f *usageFilter) from() string {
	from := "usage_events e"
	if f.source != "" {
		from = f.source + " e"
	}
	if f.NeedsKeys {
		from += " LEFT JOIN api_keys k ON k.id = e.api_key_id"
	}
	return from
}

// breakdownDims maps a group_by value to its SQL. Whitelisted rather than
// interpolated, since this lands directly in a GROUP BY.
var breakdownDims = map[string]struct{ expr, label string }{
	"model":      {"e.model_name", "coalesce(nullif(e.model_name, ''), '(none)')"},
	"provider":   {"e.provider_id::text", "coalesce(p.name, '(none)')"},
	"deployment": {"e.deployment_id::text", "coalesce(d.upstream_name, '(none)')"},
	"key":        {"e.api_key_id::text", "coalesce(k2.name, '(deleted key)')"},
	"user":       {"e.user_id::text", "coalesce(u.email, '(none)')"},
	"team":       {"e.team_id::text", "coalesce(t.name, '(none)')"},
	"status":     {"e.status_code::text", "e.status_code::text"},
	"error_code": {"coalesce(e.error_code, '')", "coalesce(nullif(e.error_code, ''), '(none)')"},
	"gateway":    {"e.gateway_id::text", "coalesce(gg.name || ' / ' || gi.name, e.gateway_id::text, '(control plane)')"},
}

// usageBreakdown aggregates the filtered rows by one dimension. This is the
// engine behind the analytics page: same filters, different grouping.
func (m *Server) usageBreakdown(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	dim, ok := breakdownDims[r.URL.Query().Get("group_by")]
	if !ok {
		httpError(w, http.StatusBadRequest, "group_by must be one of: "+strings.Join(dimNames(), ", "))
		return
	}
	f := newUsageFilter(r, c, true)
	if f.Err != nil {
		httpError(w, http.StatusBadRequest, f.Err.Error())
		return
	}
	// Name lookups are joined unconditionally here: the grouping label needs
	// them, and at breakdown cardinality the cost is irrelevant.
	m.listPercentiles(w, r, false, fmt.Sprintf(`
		SELECT %s AS bucket,
		       %s AS label,
		       coalesce(sum(e.requests), 0)::bigint            AS requests,
		       coalesce(sum(e.prompt_tokens), 0)::bigint       AS prompt_tokens,
		       coalesce(sum(e.completion_tokens), 0)::bigint   AS completion_tokens,
		       coalesce(sum(e.cached_tokens), 0)::bigint       AS cached_tokens,
		       coalesce(sum(e.cost_usd), 0)                    AS cost_usd,
		       coalesce(sum(e.notional_cost_usd), 0)           AS notional_cost_usd,
		       coalesce(sum(e.requests) FILTER (WHERE e.status_code >= 400), 0)::bigint AS errors,
		       coalesce(sum(e.requests) FILTER (WHERE e.failover), 0)::bigint           AS failovers,
		       llmr_hist_sum(e.latency_hist)                   AS latency_hist,
		       llmr_hist_sum(e.ttft_hist)                      AS ttft_hist
		FROM %s e
		LEFT JOIN api_keys k ON k.id = e.api_key_id
		LEFT JOIN api_keys k2 ON k2.id = e.api_key_id
		LEFT JOIN providers p ON p.id = e.provider_id
		LEFT JOIN model_deployments d ON d.id = e.deployment_id
		LEFT JOIN users u ON u.id = e.user_id
		LEFT JOIN teams t ON t.id = e.team_id
		LEFT JOIN gateway_instances gi ON gi.id = e.gateway_id
		LEFT JOIN gateway_groups gg ON gg.id = gi.group_id
		WHERE %s
		GROUP BY 1, 2
		ORDER BY cost_usd DESC, requests DESC
		LIMIT 100`, dim.expr, dim.label, f.source, f.where()), f.args...)
}

func dimNames() []string {
	out := make([]string, 0, len(breakdownDims))
	for k := range breakdownDims {
		out = append(out, k)
	}
	return out
}

// usageStats is the filtered headline: the same numbers the dashboard shows, but
// obeying every filter rather than only the time range.
func (m *Server) usageStats(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	f := newUsageFilter(r, c, true)
	if f.Err != nil {
		httpError(w, http.StatusBadRequest, f.Err.Error())
		return
	}
	// Percentiles are estimates read from latency histograms.
	m.listPercentiles(w, r, true, fmt.Sprintf(`
		SELECT coalesce(sum(e.requests), 0)::bigint            AS requests,
		       coalesce(sum(e.prompt_tokens), 0)::bigint       AS prompt_tokens,
		       coalesce(sum(e.completion_tokens), 0)::bigint   AS completion_tokens,
		       coalesce(sum(e.cached_tokens), 0)::bigint       AS cached_tokens,
		       coalesce(sum(e.cost_usd), 0)                    AS cost_usd,
		       coalesce(sum(e.notional_cost_usd), 0)           AS notional_cost_usd,
		       coalesce(sum(e.requests) FILTER (WHERE e.unpriced), 0)::bigint           AS unpriced_requests,
		       coalesce(sum(e.requests) FILTER (WHERE e.status_code >= 400), 0)::bigint AS errors,
		       coalesce(sum(e.requests) FILTER (WHERE e.failover), 0)::bigint           AS failovers,
		       count(DISTINCT e.model_name)                    AS models,
		       llmr_hist_sum(e.latency_hist)                   AS latency_hist,
		       llmr_hist_sum(e.ttft_hist)                      AS ttft_hist
		FROM %s
		WHERE %s`, f.from(), f.where()), f.args...)
}

// usageTimeseries buckets the filtered rows for the analytics chart. Hourly for
// short windows, daily beyond, so a 90-day view does not return 2,160 points.
func (m *Server) usageTimeseries(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	bucket := "day"
	if r.URL.Query().Get("bucket") == "hour" || daysParam(r) <= 2 {
		bucket = "hour"
	}
	f := newUsageFilter(r, c, true)
	if f.Err != nil {
		httpError(w, http.StatusBadRequest, f.Err.Error())
		return
	}
	m.list(w, r, fmt.Sprintf(`
		SELECT date_trunc('%s', e.bucket, 'UTC')            AS bucket,
		       coalesce(sum(e.requests), 0)::bigint         AS requests,
		       coalesce(sum(e.prompt_tokens + e.completion_tokens), 0)::bigint AS tokens,
		       coalesce(sum(e.cost_usd), 0)                 AS cost_usd,
		       coalesce(sum(e.requests) FILTER (WHERE e.status_code >= 400), 0)::bigint AS errors
		FROM %s
		WHERE %s
		GROUP BY 1 ORDER BY 1`, bucket, f.from(), f.where()), f.args...)
}
