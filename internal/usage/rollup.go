package usage

import (
	"context"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// histBounds split latencies into len(histBounds)+1 ranges, in milliseconds:
// below the first, between each pair, and from the last up. The same bounds
// are in llmr_hist (migration 0027); the two must agree.
var histBounds = []int{10, 15, 25, 35, 50, 75, 100, 150, 250, 350, 500, 750, 1000, 1500, 2500, 3500, 5000, 7500, 10000, 15000, 25000, 35000, 50000, 75000, 100000, 150000, 250000, 350000, 500000}

// histBucket is the index of the range ms falls in.
func histBucket(ms int) int {
	return sort.SearchInts(histBounds, ms+1)
}

// Percentile reads the p-th percentile (0..1) from histogram counts, placing
// it within its range in proportion to its rank, as Prometheus does. It is an
// estimate: exact to the range, interpolated inside it. No counts gives 0.
func Percentile(counts []int64, p float64) int {
	var total int64
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	rank := p * float64(total)
	var seen int64
	for i, c := range counts {
		if c == 0 || float64(seen+c) < rank {
			seen += c
			continue
		}
		lo := 0
		if i > 0 {
			lo = histBounds[i-1]
		}
		if i >= len(histBounds) {
			return lo // the open-ended last range
		}
		return lo + int((rank-float64(seen))/float64(c)*float64(histBounds[i]-lo))
	}
	return histBounds[len(histBounds)-1]
}

// money rounds a sum of costs to ten decimal places, the precision prices are
// kept at. Adding floats leaves noise in the last digits, and Postgres would
// store it: 44.601999999999968 for 44.602.
func money(usd float64) float64 { return math.Round(usd*1e10) / 1e10 }

// rollupKey is the dimensions one rollup row is for, less its time bucket.
type rollupKey struct {
	teamID, userID, apiKeyID, model, providerID, deploymentID, gatewayID string
	status                                                               int
	errorCode                                                            string
	stream, unpriced, failover                                           bool
}

func (k rollupKey) String() string {
	return strings.Join([]string{k.teamID, k.userID, k.apiKeyID, k.model, k.providerID, k.deploymentID, k.gatewayID,
		strconv.Itoa(k.status), k.errorCode, strconv.FormatBool(k.stream), strconv.FormatBool(k.unpriced),
		strconv.FormatBool(k.failover)}, "\x00")
}

type rollupRow struct {
	table  string
	bucket time.Time
	key    rollupKey
	sort   string

	requests, prompt, completion, cached int64
	cost, notional                       float64
	latency, ttft                        []int64 // ttft is nil until a request reports one
}

// writeRollups adds the events to the hourly and daily rollups: one upsert per
// (bucket, dimensions) in each.
func writeRollups(ctx context.Context, tx pgx.Tx, events []Event) error {
	type at struct {
		table  string
		bucket time.Time
		key    rollupKey
	}
	agg := map[at]*rollupRow{}
	for _, e := range events {
		ts := e.TS.UTC()
		key := rollupKey{e.TeamID, e.UserID, e.APIKeyID, e.ModelName, e.ProviderID, e.DeploymentID, e.GatewayID,
			e.StatusCode, e.ErrorCode, e.Stream, e.Unpriced, e.Attempts > 1}
		for _, a := range []at{
			{"usage_hourly", ts.Truncate(time.Hour), key},
			{"usage_daily", ts.Truncate(24 * time.Hour), key},
		} {
			r := agg[a]
			if r == nil {
				r = &rollupRow{table: a.table, bucket: a.bucket, key: key, latency: make([]int64, len(histBounds)+1)}
				r.sort = a.table + "\x00" + a.bucket.Format(time.RFC3339) + "\x00" + key.String()
				agg[a] = r
			}
			r.requests++
			r.prompt += int64(e.PromptTokens)
			r.completion += int64(e.CompletionTokens)
			r.cached += int64(e.CachedTokens)
			r.cost += e.CostUSD
			r.notional += e.NotionalCostUSD
			r.latency[histBucket(e.LatencyMS)]++
			if e.TTFTMs > 0 {
				if r.ttft == nil {
					r.ttft = make([]int64, len(histBounds)+1)
				}
				r.ttft[histBucket(e.TTFTMs)]++
			}
		}
	}
	if len(agg) == 0 {
		return nil
	}
	// One fixed order, as in writeCounters, so concurrent writers that share a
	// row cannot deadlock.
	rows := make([]*rollupRow, 0, len(agg))
	for _, r := range agg {
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(a, b *rollupRow) int { return strings.Compare(a.sort, b.sort) })

	b := &pgx.Batch{}
	for _, r := range rows {
		k := r.key
		// the table name is one of the two above, not input
		b.Queue(`INSERT INTO `+r.table+` AS t
			(bucket, team_id, user_id, api_key_id, model_name, provider_id, deployment_id, gateway_id,
			 status_code, error_code, stream, unpriced, failover,
			 requests, prompt_tokens, completion_tokens, cached_tokens, cost_usd, notional_cost_usd,
			 latency_hist, ttft_hist)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
			ON CONFLICT (bucket, team_id, user_id, api_key_id, model_name, provider_id, deployment_id, gateway_id,
			             status_code, error_code, stream, unpriced, failover)
			DO UPDATE SET requests = t.requests + EXCLUDED.requests,
			              prompt_tokens = t.prompt_tokens + EXCLUDED.prompt_tokens,
			              completion_tokens = t.completion_tokens + EXCLUDED.completion_tokens,
			              cached_tokens = t.cached_tokens + EXCLUDED.cached_tokens,
			              cost_usd = t.cost_usd + EXCLUDED.cost_usd,
			              notional_cost_usd = t.notional_cost_usd + EXCLUDED.notional_cost_usd,
			              latency_hist = llmr_hist_add(t.latency_hist, EXCLUDED.latency_hist),
			              ttft_hist = llmr_hist_add(t.ttft_hist, EXCLUDED.ttft_hist)`,
			r.bucket, nilIfEmpty(k.teamID), nilIfEmpty(k.userID), nilIfEmpty(k.apiKeyID), k.model,
			nilIfEmpty(k.providerID), nilIfEmpty(k.deploymentID), nilIfEmpty(k.gatewayID),
			k.status, nilIfEmpty(k.errorCode), k.stream, k.unpriced, k.failover,
			r.requests, r.prompt, r.completion, r.cached, money(r.cost), money(r.notional), r.latency, r.ttft)
	}
	return tx.SendBatch(ctx, b).Close()
}
