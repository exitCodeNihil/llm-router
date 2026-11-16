package mgmt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// The aggregate endpoints read rollups. They must give the numbers a query
// over the raw events gives, for ranges that end inside an hour, a day and a
// month. It needs a disposable database:
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/mgmt
func TestAggregatesMatchRawEvents(t *testing.T) {
	url := os.Getenv("LLMR_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LLMR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := usage.NewPGWriter(st.Pool).EnsurePartitions(ctx); err != nil {
		t.Fatal(err)
	}

	team, now := uuid.NewString(), time.Now().UTC()
	var events []usage.Event
	var oneRequest string
	for i, ago := range []time.Duration{10 * time.Minute, 26 * time.Hour, 74 * time.Hour, 482 * time.Hour} {
		for j := range 5 {
			e := usage.Event{TS: now.Add(-ago), RequestID: uuid.NewString(), TeamID: team, ModelName: "m" + string(rune('a'+j%2)),
				PromptTokens: 100 * (i + 1), CompletionTokens: 10 + j, CachedTokens: j, CostUSD: 0.25, NotionalCostUSD: 0.5,
				LatencyMS: 300 + 700*j, TTFTMs: 120, StatusCode: 200, Stream: true, Attempts: 1}
			if j == 4 {
				e.StatusCode, e.ErrorCode, e.Attempts, e.TTFTMs, e.Unpriced = 502, "upstream_error", 2, 0, true
			}
			events = append(events, e)
			oneRequest = e.RequestID
		}
	}
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := usage.InsertEvents(ctx, tx, events); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{})
	m := &Server{Store: st, AdminToken: "test-admin-token", EncryptionKey: "test-encryption-key-0123456789", Snapshots: holder}
	mux := http.NewServeMux()
	m.Register(mux)
	get := func(path string, into any) {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer test-admin-token")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatalf("GET %s: %v in %s", path, err, rec.Body.String())
		}
	}
	num := func(v any) float64 {
		switch n := v.(type) {
		case float64:
			return n
		case string: // numeric columns
			var f float64
			json.Unmarshal([]byte(n), &f)
			return f
		}
		return -1
	}

	for _, days := range []string{"1", "7", "30"} {
		var raw struct {
			requests, prompt, completion, cached, errors, failovers, unpriced, models float64
			cost, notional                                                            float64
		}
		if err := st.Pool.QueryRow(ctx, `
			SELECT count(*), sum(prompt_tokens), sum(completion_tokens), sum(cached_tokens),
			       count(*) FILTER (WHERE status_code >= 400), count(*) FILTER (WHERE attempts > 1),
			       count(*) FILTER (WHERE unpriced), count(DISTINCT model_name),
			       sum(cost_usd)::float8, sum(notional_cost_usd)::float8
			FROM usage_events WHERE team_id = $1 AND ts >= now() - make_interval(days => $2::int)`, team, days).
			Scan(&raw.requests, &raw.prompt, &raw.completion, &raw.cached, &raw.errors, &raw.failovers,
				&raw.unpriced, &raw.models, &raw.cost, &raw.notional); err != nil {
			t.Fatal(err)
		}
		q := "?team_id=" + team + "&days=" + days

		var stats map[string]any
		get("/api/usage/stats"+q, &stats)
		for k, want := range map[string]float64{"requests": raw.requests, "prompt_tokens": raw.prompt,
			"completion_tokens": raw.completion, "cached_tokens": raw.cached, "errors": raw.errors,
			"failovers": raw.failovers, "unpriced_requests": raw.unpriced, "models": raw.models,
			"cost_usd": raw.cost, "notional_cost_usd": raw.notional} {
			if got := num(stats[k]); got != want {
				t.Errorf("days=%s stats.%s = %v, want %v", days, k, stats[k], want)
			}
		}
		// Latencies are 300, 1000, 1700, 2400 and 3100 ms in equal numbers:
		// the median is 1700 and the 95th percentile 3100. Percentiles are
		// read from histograms, so they are right to the histogram's range.
		if p := num(stats["p50_ms"]); p < 1500 || p > 2500 {
			t.Errorf("days=%s p50_ms = %v, want within 1500-2500", days, p)
		}
		if p := num(stats["p95_ms"]); p < 2500 || p > 3500 {
			t.Errorf("days=%s p95_ms = %v, want within 2500-3500", days, p)
		}
		if p := num(stats["ttft_p50_ms"]); p < 100 || p > 150 {
			t.Errorf("days=%s ttft_p50_ms = %v, want within 100-150", days, p)
		}

		var summary map[string]any
		get("/api/usage/summary"+q, &summary)
		if got := num(summary["requests"]); got != raw.requests {
			t.Errorf("days=%s summary.requests = %v, want %v", days, got, raw.requests)
		}
		if got := num(summary["tokens"]); got != raw.prompt+raw.completion {
			t.Errorf("days=%s summary.tokens = %v, want %v", days, got, raw.prompt+raw.completion)
		}

		// Each list adds up to the same total, however it is split.
		for _, path := range []string{"/api/usage/by-model", "/api/usage/timeseries", "/api/usage/timeseries" + "?bucket=hour&x=", "/api/usage/breakdown?group_by=status&x="} {
			var rows []map[string]any
			if path[len(path)-1] == '=' {
				get(path+"&"+q[1:], &rows)
			} else {
				get(path+q, &rows)
			}
			var requests, cost float64
			for _, row := range rows {
				requests += num(row["requests"])
				cost += num(row["cost_usd"])
			}
			if requests != raw.requests || cost != raw.cost {
				t.Errorf("days=%s %s adds up to %v requests, $%v; want %v, $%v", days, path, requests, cost, raw.requests, raw.cost)
			}
		}
	}

	// One request by id comes from its raw event.
	var one map[string]any
	get("/api/usage/stats?request_id="+oneRequest, &one)
	if num(one["requests"]) != 1 || num(one["errors"]) != 1 {
		t.Errorf("stats for one request = %v", one)
	}
	// A filter on failover reads the rollups' own column.
	var failed map[string]any
	get("/api/usage/stats?failover=true&team_id="+team, &failed)
	if num(failed["requests"]) != 4 {
		t.Errorf("failover requests = %v, want 4", failed["requests"])
	}
}
