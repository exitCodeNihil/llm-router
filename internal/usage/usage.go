// Package usage captures per-request usage events and writes them to Postgres
// in batches. The Writer interface is the seam for the edge shipper and a
// future ClickHouse writer.
package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Event struct {
	TS               time.Time `json:"ts"`
	RequestID        string    `json:"request_id"`
	APIKeyID         string    `json:"api_key_id,omitempty"`
	UserID           string    `json:"user_id,omitempty"`
	TeamID           string    `json:"team_id,omitempty"`
	ModelName        string    `json:"model_name"`
	DeploymentID     string    `json:"deployment_id,omitempty"`
	ProviderID       string    `json:"provider_id,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CachedTokens     int       `json:"cached_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	Unpriced         bool      `json:"unpriced"`
	LatencyMS        int       `json:"latency_ms"`
	StatusCode       int       `json:"status_code"`
	Stream           bool      `json:"stream"`
	EdgeNodeID       string    `json:"edge_node_id,omitempty"`

	// Attempts is 1 when the first-choice deployment answered, higher after
	// failover. ErrorCode is the gateway's own reason for a failure, which
	// StatusCode cannot distinguish. TTFTMs is time to first streamed token.
	// NotionalCostUSD is what the request would have cost at list rates. Set only
	// when CostUSD is not real money (subscription pass-through); never summed
	// into spend.
	NotionalCostUSD float64 `json:"notional_cost_usd,omitempty"`

	Attempts  int    `json:"attempts,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	TTFTMs    int    `json:"ttft_ms,omitempty"`

	// Tags aggregated from the key/user/team, for telemetry scoping.
	Tags []string `json:"tags,omitempty"`

	// Telemetry export decision, computed once post-auth in the gateway and
	// carried through (incl. over the edge wire). Honored by exporters; never
	// written to Postgres.
	Export         bool   `json:"export,omitempty"`
	CaptureContent bool   `json:"capture_content,omitempty"`
	RuleID         string `json:"rule_id,omitempty"`

	// Optional message content, present only when telemetry content capture is
	// enabled. Consumed by telemetry exporters; never written to Postgres.
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
}

type Writer interface {
	Write(e Event) // must never block the request path
}

// MultiWriter fans an event out to several writers (Postgres + exporters).
type MultiWriter []Writer

func (m MultiWriter) Write(e Event) {
	for _, w := range m {
		w.Write(e)
	}
}

// PGWriter batches events into Postgres: multi-row insert + spend_counters
// upsert in one tx. A full buffer drops the oldest event rather than blocking.
type PGWriter struct {
	pool    *pgxpool.Pool
	ch      chan Event
	Dropped atomic.Int64
}

func NewPGWriter(pool *pgxpool.Pool) *PGWriter {
	return &PGWriter{pool: pool, ch: make(chan Event, 65536)}
}

const flushBatchSize = 1000

func (w *PGWriter) Write(e Event) {
	for {
		select {
		case w.ch <- e:
			return
		default:
			select {
			case <-w.ch: // drop oldest
				w.Dropped.Add(1)
			default:
			}
		}
	}
}

// Run consumes events until ctx ends, flushing every 100 events or 1s.
// It also pre-creates monthly partitions.
func (w *PGWriter) Run(ctx context.Context) {
	if err := w.EnsurePartitions(ctx); err != nil {
		slog.Error("usage partitions", "err", err)
	}
	partTicker := time.NewTicker(12 * time.Hour)
	defer partTicker.Stop()

	flushTicker := time.NewTicker(time.Second)
	defer flushTicker.Stop()

	batch := make([]Event, 0, flushBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := w.flush(ctx, batch); err != nil && ctx.Err() == nil {
			slog.Error("usage flush failed", "events", len(batch), "err", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// final flush with a fresh context so shutdown persists buffered events
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			for {
				select {
				case e := <-w.ch:
					batch = append(batch, e)
					continue
				default:
				}
				break
			}
			if len(batch) > 0 {
				if err := w.flush(fctx, batch); err != nil {
					slog.Error("final usage flush failed", "err", err)
				}
			}
			cancel()
			return
		case e := <-w.ch:
			batch = append(batch, e)
			if len(batch) >= flushBatchSize {
				flush()
			}
		case <-flushTicker.C:
			flush()
		case <-partTicker.C:
			w.ensurePartitions(ctx, time.Now().UTC())
		}
	}
}

func (w *PGWriter) flush(ctx context.Context, events []Event) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	type counter struct {
		scopeType, scopeID string
		day                time.Time
	}
	agg := map[counter]*struct {
		usd    float64
		tokens int64
	}{}

	b := &pgx.Batch{}
	for _, e := range events {
		b.Queue(`INSERT INTO usage_events
			(ts, request_id, api_key_id, user_id, team_id, model_name, deployment_id, provider_id,
			 prompt_tokens, completion_tokens, cached_tokens, cost_usd, unpriced, latency_ms, status_code, stream, edge_node_id,
			 attempts, error_code, ttft_ms, notional_cost_usd)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
			e.TS, e.RequestID, nilIfEmpty(e.APIKeyID), nilIfEmpty(e.UserID), nilIfEmpty(e.TeamID),
			e.ModelName, nilIfEmpty(e.DeploymentID), nilIfEmpty(e.ProviderID),
			e.PromptTokens, e.CompletionTokens, e.CachedTokens, e.CostUSD, e.Unpriced,
			e.LatencyMS, e.StatusCode, e.Stream, nilIfEmpty(e.EdgeNodeID),
			zeroIfNil(e.Attempts), nilIfEmpty(e.ErrorCode), zeroIfNil(e.TTFTMs), zeroFloatIfNil(e.NotionalCostUSD))

		day := e.TS.UTC().Truncate(24 * time.Hour)
		tokens := int64(e.PromptTokens + e.CompletionTokens)
		scopes := map[string]string{"key": e.APIKeyID, "user": e.UserID, "team": e.TeamID}
		if e.UserID != "" && e.TeamID != "" {
			scopes["member"] = e.UserID + ":" + e.TeamID // a member's share of the team pool
		}
		for scopeType, scopeID := range scopes {
			if scopeID == "" {
				continue
			}
			k := counter{scopeType, scopeID, day}
			c := agg[k]
			if c == nil {
				c = &struct {
					usd    float64
					tokens int64
				}{}
				agg[k] = c
			}
			c.usd += e.CostUSD
			c.tokens += tokens
		}
	}
	// one aggregated upsert per (scope, day) instead of three per event
	for k, c := range agg {
		b.Queue(`INSERT INTO spend_counters (scope_type, scope_id, period_start, usd, tokens)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (scope_type, scope_id, period_start)
			DO UPDATE SET usd = spend_counters.usd + EXCLUDED.usd,
			              tokens = spend_counters.tokens + EXCLUDED.tokens`,
			k.scopeType, k.scopeID, k.day, c.usd, c.tokens)
	}
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EnsurePartitions creates last, this and next month's usage_events
// partitions. Called once at startup, where a failure is fatal: without a
// partition every usage row is silently dropped while the gateway reports
// healthy. Last month is included so late-arriving edge batches still land.
func (w *PGWriter) EnsurePartitions(ctx context.Context) error {
	return w.ensurePartitions(ctx, time.Now().UTC())
}

func (w *PGWriter) ensurePartitions(ctx context.Context, now time.Time) error {
	cur := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var firstErr error
	for _, start := range []time.Time{cur.AddDate(0, -1, 0), cur, cur.AddDate(0, 1, 0)} {
		end := start.AddDate(0, 1, 0)
		name := "usage_events_" + start.Format("2006_01")
		// DDL cannot take bind params; bounds are generated, not user input
		_, err := w.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+name+
			` PARTITION OF usage_events FOR VALUES FROM ('`+start.Format("2006-01-02")+
			`') TO ('`+end.Format("2006-01-02")+`')`)
		if err != nil {
			slog.Error("create partition failed", "partition", name, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("create partition %s: %w", name, err)
			}
			continue
		}
		for _, idx := range []string{
			`CREATE INDEX IF NOT EXISTS ` + name + `_team_ts ON ` + name + ` (team_id, ts)`,
			`CREATE INDEX IF NOT EXISTS ` + name + `_key_ts ON ` + name + ` (api_key_id, ts)`,
			// ts-only index powers the Requests feed / by-deployment scans.
			// ponytail: applied to new partitions only; old dev partitions stay small.
			`CREATE INDEX IF NOT EXISTS ` + name + `_ts ON ` + name + ` (ts DESC)`,
		} {
			if _, err := w.pool.Exec(ctx, idx); err != nil {
				slog.Error("create partition index failed", "partition", name, "err", err)
			}
		}
	}
	return firstErr
}

// zeroIfNil keeps NULL for unmeasured values, so a missing TTFT is not read as
// an instantaneous first token.
func zeroIfNil(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// zeroFloatIfNil keeps NULL for "not applicable" so a priced request and an
// unvalued one are distinguishable from a genuine zero.
func zeroFloatIfNil(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
