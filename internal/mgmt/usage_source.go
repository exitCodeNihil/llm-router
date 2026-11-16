package mgmt

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/exitcodenihil/llm-router/internal/usage"
)

// rollupCols are the columns every aggregate usage query reads, from the
// rollup tables or from raw events shaped like them.
const rollupCols = `bucket, team_id, user_id, api_key_id, model_name, provider_id, deployment_id, gateway_id,
	status_code, error_code, stream, unpriced, failover, requests, prompt_tokens, completion_tokens,
	cached_tokens, cost_usd, notional_cost_usd, latency_hist, ttft_hist`

// usageSource returns a FROM item, to be aliased, holding the usage between
// since and end. It reads whole days from usage_daily and the part-days at
// either end from usage_hourly, so the range is right to the hour without
// summing hourly rows across months. hourly reads every hour from
// usage_hourly, for a chart with hourly points. next reserves a placeholder.
//
// With a requestID it reads that request's raw events instead: rollups do not
// keep request ids.
func usageSource(next func(any) string, since, end time.Time, hourly bool, requestID string) string {
	if requestID != "" {
		return `(SELECT ts AS bucket, team_id, user_id, api_key_id, model_name, provider_id, deployment_id, gateway_id,
			status_code, error_code, stream, unpriced, coalesce(attempts, 0) > 1 AS failover, 1::bigint AS requests,
			prompt_tokens::bigint AS prompt_tokens, completion_tokens::bigint AS completion_tokens,
			cached_tokens::bigint AS cached_tokens, cost_usd, coalesce(notional_cost_usd, 0) AS notional_cost_usd,
			llmr_hist(latency_ms) AS latency_hist, llmr_hist(ttft_ms) AS ttft_hist
			FROM usage_events WHERE ts >= ` + next(since) + ` AND ts <= ` + next(end) + ` AND request_id = ` + next(requestID) + `)`
	}
	const day = 24 * time.Hour
	since, end = since.UTC(), end.UTC()
	// Hourly rollups older than this have been dropped.
	hourlyFrom := usage.HourlyFrom(time.Now())
	h1 := since.Truncate(time.Hour)
	d1, d2 := since.Truncate(day), end.Truncate(day) // the whole days: [d1, d2)
	if d1.Before(since) {
		d1 = d1.Add(day)
	}

	var parts []string
	part := func(table string, lo, hi time.Time, upTo string) {
		parts = append(parts, `SELECT `+rollupCols+` FROM `+table+
			` WHERE bucket >= `+next(lo)+` AND bucket `+upTo+` `+next(hi))
	}
	switch {
	case h1.Before(hourlyFrom):
		// Too old for hourly rows: whole days, from the day since falls in.
		part("usage_daily", since.Truncate(day), end, "<=")
	case hourly || !d1.Before(d2):
		part("usage_hourly", h1, end, "<=")
	default:
		if h1.Before(d1) {
			part("usage_hourly", h1, d1, "<")
		}
		part("usage_daily", d1, d2, "<")
		part("usage_hourly", d2, end, "<=")
	}
	return `(` + strings.Join(parts, ` UNION ALL `) + `)`
}

// placeholder appends an argument and returns its $n.
func placeholder(args *[]any, v any) string {
	*args = append(*args, v)
	return "$" + strconv.Itoa(len(*args))
}

// daysSource is usageSource for the last ?days.
func daysSource(args *[]any, days int) string {
	now := time.Now()
	return usageSource(func(v any) string { return placeholder(args, v) }, now.AddDate(0, 0, -days), now, false, "")
}

// listPercentiles is list for rows that carry latency_hist and ttft_hist: it
// replaces the histograms with the percentiles read from them.
func (m *Server) listPercentiles(w http.ResponseWriter, r *http.Request, one bool, sql string, args ...any) {
	rows, err := listRows(r.Context(), m.reads(), sql, args...)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, row := range rows {
		latency, ttft := histCounts(row["latency_hist"]), histCounts(row["ttft_hist"])
		delete(row, "latency_hist")
		delete(row, "ttft_hist")
		row["p50_ms"] = usage.Percentile(latency, 0.5)
		row["p95_ms"] = usage.Percentile(latency, 0.95)
		row["ttft_p50_ms"] = usage.Percentile(ttft, 0.5)
	}
	switch {
	case !one:
		writeJSON(w, http.StatusOK, rows)
	case len(rows) == 0:
		writeJSON(w, http.StatusOK, map[string]any{})
	default:
		writeJSON(w, http.StatusOK, rows[0])
	}
}

// histCounts reads a bigint[] column as the driver returns it.
func histCounts(v any) []int64 {
	vals, _ := v.([]any)
	out := make([]int64, 0, len(vals))
	for _, x := range vals {
		n, _ := x.(int64)
		out = append(out, n)
	}
	return out
}
