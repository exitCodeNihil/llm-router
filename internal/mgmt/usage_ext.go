package mgmt

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// windowDays parses ?window=today|Nd into a lower-bound day count (today = 1).
func windowDays(r *http.Request) int {
	w := r.URL.Query().Get("window")
	if w == "" || w == "today" {
		return 1
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(w, "d")); err == nil && n >= 1 && n <= 365 {
		return n
	}
	return 1
}

// usageByTeam: spend_counters rolled up per team over the window (today | Nd).
// Admins see all teams; members only their own. Cheap — a straight WHERE.
func (m *Server) usageByTeam(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	days := windowDays(r)
	since := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	args := []any{since}
	teamScope := "TRUE"
	if !c.IsAdmin {
		args = append(args, c.UserID)
		teamScope = `sc.scope_id = ANY(SELECT team_id::text FROM team_members WHERE user_id = $2)`
	}
	m.list(w, r, `
		SELECT sc.scope_id AS team_id, t.name AS team_name,
		       coalesce(sum(sc.usd), 0) AS usd,
		       coalesce(sum(sc.tokens), 0) AS tokens
		FROM spend_counters sc JOIN teams t ON t.id::text = sc.scope_id
		WHERE sc.scope_type = 'team' AND sc.period_start >= $1 AND `+teamScope+`
		GROUP BY 1, 2 ORDER BY usd DESC`, args...)
}

// usageByProviderDaily: daily spend split by provider, long-form rows the
// dashboard pivots into a stacked chart. Mirrors usageDaily + a provider join.
func (m *Server) usageByProviderDaily(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	var args []any
	source := daysSource(&args, daysParam(r))
	scope := usageScope(c, &args)
	m.list(w, r, fmt.Sprintf(`
		SELECT (e.bucket AT TIME ZONE 'UTC')::date AS day,
		       coalesce(p.name, 'other') AS provider,
		       coalesce(sum(e.cost_usd), 0) AS cost_usd
		FROM %s e LEFT JOIN providers p ON p.id = e.provider_id
		WHERE %s
		GROUP BY 1, 2 ORDER BY 1`, source, scope), args...)
}

// usageRequests is the enriched, filterable, cursor-paginated request feed.
func (m *Server) usageRequests(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())

	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit < 1 || limit > 500 {
		limit = 50
	}

	// Same filter the aggregates use, so a row list and a headline number always
	// describe the same set.
	f := newUsageFilter(r, c, false)
	if f.Err != nil {
		httpError(w, http.StatusBadRequest, f.Err.Error())
		return
	}
	where := f.where()

	// cursor: before=<rfc3339 ts>,<id>
	if cur := r.URL.Query().Get("before"); cur != "" {
		if ts, id, ok := parseCursor(cur); ok {
			where += ` AND (e.ts, e.id) < (` + f.next(ts) + `, ` + f.next(id) + `)`
		}
	}
	args := append(f.args, limit)

	m.list(w, r, `
		SELECT e.id, e.ts, e.request_id, e.model_name, e.api_key_id, e.user_id, e.team_id,
		       e.deployment_id, e.provider_id,
		       k.name AS key_name, p.name AS provider_name, d.upstream_name AS upstream_name,
		       e.prompt_tokens, e.completion_tokens, e.cost_usd, e.latency_ms, e.status_code, e.stream,
		       e.attempts, e.error_code, e.ttft_ms, e.notional_cost_usd, e.trace_id
		FROM usage_events e
		LEFT JOIN api_keys k ON k.id = e.api_key_id
		LEFT JOIN providers p ON p.id = e.provider_id
		LEFT JOIN model_deployments d ON d.id = e.deployment_id
		WHERE `+where+`
		ORDER BY e.ts DESC, e.id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
}

func parseCursor(s string) (time.Time, int64, bool) {
	i := strings.LastIndex(s, ",")
	if i < 0 {
		return time.Time{}, 0, false
	}
	ts, err := time.Parse(time.RFC3339, s[:i])
	if err != nil {
		return time.Time{}, 0, false
	}
	id, err := strconv.ParseInt(s[i+1:], 10, 64)
	if err != nil {
		return time.Time{}, 0, false
	}
	return ts, id, true
}

// usageByDeployment: request counts per deployment over ?days=N (default 1).
func (m *Server) usageByDeployment(w http.ResponseWriter, r *http.Request) {
	days := 1
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d >= 1 && d <= 365 {
		days = d
	}
	var args []any
	source := daysSource(&args, days)
	m.list(w, r, fmt.Sprintf(`
		SELECT deployment_id, sum(requests)::bigint AS requests
		FROM %s e
		WHERE deployment_id IS NOT NULL
		GROUP BY 1`, source), args...)
}
