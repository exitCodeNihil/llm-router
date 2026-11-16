// Package usage captures per-request usage events and writes them to Postgres
// in batches. The Writer interface is the seam for exporters and a future
// ClickHouse writer.
package usage

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exitcodenihil/llm-router/internal/store"
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
	CacheWrite5m     int       `json:"cache_write_5m_tokens,omitempty"`
	CacheWrite1h     int       `json:"cache_write_1h_tokens,omitempty"`
	CostUSD          float64   `json:"cost_usd"`
	Unpriced         bool      `json:"unpriced"`
	LatencyMS        int       `json:"latency_ms"`
	StatusCode       int       `json:"status_code"`
	Stream           bool      `json:"stream"`
	GatewayID        string    `json:"gateway_id,omitempty"` // the gateway instance that served it; "" = the control plane
	// GatewayGroupID is the instance's group, set by the control plane when a
	// gateway syncs; "" = the control plane. Telemetry scopes use it; not stored.
	GatewayGroupID string `json:"gateway_group_id,omitempty"`
	// ClientIP is the caller's address, set only when LLMR_USAGE_CLIENT_IP is
	// on: it is personal data.
	ClientIP string `json:"client_ip,omitempty"`

	// TraceID (32 hex) comes from the caller's traceparent or is new;
	// ParentSpanID (16 hex) is the caller's span, not stored.
	TraceID      string `json:"trace_id,omitempty"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
	// InferenceGeo and Speed are what an Anthropic upstream reported. CostBasis
	// is the pricing CostUSD was computed with (CostBasisCurrent); 0 = unknown.
	InferenceGeo string `json:"inference_geo,omitempty"`
	// Operation is gen_ai.operation.name; "" = chat. Not stored.
	Operation string `json:"operation,omitempty"`
	Speed     string `json:"speed,omitempty"`
	CostBasis int    `json:"cost_basis,omitempty"`

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
	// carried through. Honored by exporters; never
	// written to Postgres.
	Export         bool   `json:"export,omitempty"`
	CaptureContent bool   `json:"capture_content,omitempty"`
	RuleID         string `json:"rule_id,omitempty"`

	// Optional message content, present only when telemetry content capture is
	// enabled. Consumed by telemetry exporters; never written to Postgres.
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
	System json.RawMessage `json:"system,omitempty"` // Anthropic's top-level system
	// ContentTruncated is set when captured content was cut to the cap.
	ContentTruncated bool `json:"content_truncated,omitempty"`
}

// CostBasisCurrent marks costs computed with 0.3.0's pricing.
const CostBasisCurrent = 1

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
	// FlushFailures counts failed writes to Postgres; each is retried.
	FlushFailures atomic.Int64
	// Lost counts events given up on: those still unwritten at shutdown.
	Lost atomic.Int64

	// Retention is what Prune keeps. Set before Run.
	Retention Retention
	// PartitionsDropped counts monthly partitions dropped past their retention.
	PartitionsDropped atomic.Int64
	// PartitionErrors counts monthly partitions that could not be created.
	PartitionErrors atomic.Int64
	// Maintenance runs after Prune in each maintenance pass, under the same
	// lock. Set before Run.
	Maintenance []func(context.Context) error
	// Committed is told of the events of each batch once Postgres holds
	// them, and their spend with them. Set before Run.
	Committed func([]Event)

	indexing sync.Mutex // one EnsureIndexes pass at a time
}

func NewPGWriter(pool *pgxpool.Pool) *PGWriter {
	return &PGWriter{pool: pool, ch: make(chan Event, 65536)}
}

const flushBatchSize = 1000

// Queued is the events waiting to be written, of Capacity.
func (w *PGWriter) Queued() int   { return len(w.ch) }
func (w *PGWriter) Capacity() int { return cap(w.ch) }

func (w *PGWriter) Write(e Event) {
	// Content is never stored; dropping it here keeps up to 65k queued events
	// from holding on to request and response bodies.
	e.Input, e.Output, e.System = nil, "", nil
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

// Run consumes events until ctx ends, flushing every 1000 events or 1s.
// It also pre-creates monthly partitions.
func (w *PGWriter) Run(ctx context.Context) {
	if err := w.EnsurePartitions(ctx); err != nil {
		slog.Error("usage partitions", "err", err)
	}
	// In the background: on a large partition an index takes a while to build.
	go w.maintain(ctx)
	partTicker := time.NewTicker(12 * time.Hour)
	defer partTicker.Stop()

	flushTicker := time.NewTicker(time.Second)
	defer flushTicker.Stop()

	batch := make([]Event, 0, flushBatchSize)
	// A failed flush keeps its batch and tries again, backing off to 30 s.
	// Meanwhile the queue takes new events, and drops the oldest once full.
	var retryAt time.Time
	backoff := time.Second
	flush := func() {
		if len(batch) == 0 || time.Now().Before(retryAt) {
			return
		}
		if err := w.flush(ctx, batch); err != nil {
			if ctx.Err() != nil {
				return
			}
			w.FlushFailures.Add(1)
			slog.Error("usage flush failed, will retry", "events", len(batch), "retry_in", backoff, "err", err)
			retryAt = time.Now().Add(backoff)
			backoff = min(2*backoff, 30*time.Second)
			return
		}
		batch, backoff, retryAt = batch[:0], time.Second, time.Time{}
	}

	for {
		in := w.ch
		if len(batch) >= flushBatchSize {
			in = nil // a full batch is waiting for its retry
		}
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
					w.Lost.Add(int64(len(batch)))
					slog.Error("final usage flush failed", "lost", len(batch), "err", err)
				}
			}
			cancel()
			return
		case e := <-in:
			batch = append(batch, e)
			if len(batch) >= flushBatchSize {
				flush()
			}
		case <-flushTicker.C:
			flush()
		case <-partTicker.C:
			w.ensurePartitions(ctx, time.Now().UTC())
			go w.maintain(ctx)
		}
	}
}

func (w *PGWriter) flush(ctx context.Context, events []Event) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	kept, err := InsertEvents(ctx, tx, events)
	if err != nil {
		return err
	}
	if n := len(events) - len(kept); n > 0 {
		slog.Warn("usage rows refused", "refused", n, "of", len(events))
	}
	if err := tx.Commit(ctx); err != nil {
		// The connection can fail after Postgres committed. Writing the batch
		// again would count its usage and spend twice, so look for it first.
		if len(kept) > 0 && w.stored(kept[0]) {
			w.committed(kept)
			return nil
		}
		return err
	}
	w.committed(kept)
	return nil
}

func (w *PGWriter) committed(events []Event) {
	if w.Committed != nil && len(events) > 0 {
		w.Committed(events)
	}
}

// stored reports whether e is in usage_events.
func (w *PGWriter) stored(e Event) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var found bool
	err := w.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_events WHERE request_id = $1 AND ts = $2)`,
		e.RequestID, e.TS).Scan(&found)
	return err == nil && found
}

// ScopeKeys lists the spend scopes an event counts against: its key, user and
// team, and the member's share of the team pool. They match snapshot.Spend keys.
func ScopeKeys(e Event) []string {
	var out []string
	if e.APIKeyID != "" {
		out = append(out, "key:"+e.APIKeyID)
	}
	if e.UserID != "" {
		out = append(out, "user:"+e.UserID)
	}
	if e.TeamID != "" {
		out = append(out, "team:"+e.TeamID)
		if e.UserID != "" {
			out = append(out, "member:"+e.UserID+":"+e.TeamID)
		}
	}
	return out
}

// InsertEvents writes events and their spend_counters in tx: one multi-row
// batch plus one aggregated upsert per (scope, day). It returns the events it
// stored. When Postgres refuses a row's data (error classes 22 and 23), the
// batch is split in halves until the refused rows are found and skipped, so
// one bad row cannot sink the rest; skipped rows add no spend. Any other
// error, a missing partition included, is returned so the caller can retry.
func InsertEvents(ctx context.Context, tx pgx.Tx, events []Event) ([]Event, error) {
	kept := make([]Event, 0, len(events))
	var refused []Event
	var insert func([]Event) error
	insert = func(part []Event) error {
		err := savepoint(ctx, tx, func(sp pgx.Tx) error {
			b := &pgx.Batch{}
			for _, e := range part {
				queueRow(b, e)
			}
			return sp.SendBatch(ctx, b).Close()
		})
		switch {
		case err == nil:
			kept = append(kept, part...)
			return nil
		case !refusedRow(err):
			return err
		case len(part) == 1:
			refused = append(refused, part[0])
			if len(refused) == 1 {
				slog.Warn("usage row refused", "request_id", part[0].RequestID, "ts", part[0].TS, "err", err)
			}
			return nil
		}
		half := len(part) / 2
		if err := insert(part[:half]); err != nil {
			return err
		}
		return insert(part[half:])
	}
	if len(events) > 0 {
		if err := insert(events); err != nil {
			return nil, err
		}
	}
	if len(refused) > 1 {
		slog.Warn("usage rows refused", "refused", len(refused), "of", len(events))
	}
	if err := writeCounters(ctx, tx, kept); err != nil {
		return nil, err
	}
	return kept, writeRollups(ctx, tx, kept)
}

// savepoint runs fn in a nested transaction, so a failure rolls back only
// what fn did and leaves tx usable.
func savepoint(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(sp); err != nil {
		sp.Rollback(ctx)
		return err
	}
	return sp.Commit(ctx)
}

// refusedRow reports whether Postgres refused a row's data (classes 22, data
// exception, and 23, integrity constraint violation), as opposed to a failure
// retrying could fix. A missing partition (23514) is not the row's fault:
// timestamps are clamped into the partition window, so it means partitions
// failed to be created, and the caller should retry rather than lose the row.
func refusedRow(err error) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || len(pg.Code) < 2 || pg.Code == "23514" {
		return false
	}
	return pg.Code[:2] == "22" || pg.Code[:2] == "23"
}

func queueRow(b *pgx.Batch, e Event) {
	b.Queue(`INSERT INTO usage_events
		(ts, request_id, api_key_id, user_id, team_id, model_name, deployment_id, provider_id,
		 prompt_tokens, completion_tokens, cached_tokens, cost_usd, unpriced, latency_ms, status_code, stream, gateway_id,
		 attempts, error_code, ttft_ms, notional_cost_usd, cache_write_5m_tokens, cache_write_1h_tokens,
		 trace_id, inference_geo, speed, cost_basis, client_ip)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28)`,
		e.TS, e.RequestID, nilIfEmpty(e.APIKeyID), nilIfEmpty(e.UserID), nilIfEmpty(e.TeamID),
		e.ModelName, nilIfEmpty(e.DeploymentID), nilIfEmpty(e.ProviderID),
		e.PromptTokens, e.CompletionTokens, e.CachedTokens, e.CostUSD, e.Unpriced,
		e.LatencyMS, e.StatusCode, e.Stream, nilIfEmpty(e.GatewayID),
		zeroIfNil(e.Attempts), nilIfEmpty(e.ErrorCode), zeroIfNil(e.TTFTMs), zeroFloatIfNil(e.NotionalCostUSD),
		e.CacheWrite5m, e.CacheWrite1h,
		nilIfEmpty(e.TraceID), nilIfEmpty(e.InferenceGeo), nilIfEmpty(e.Speed), zeroIfNil(e.CostBasis),
		nilIfEmpty(e.ClientIP))
}

// TotalStart is the period_start of a scope's all-time row in spend_totals.
var TotalStart = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// writeCounters adds the events' spend: one upsert per (scope, day) in
// spend_counters, and the scope's month and all-time rows in spend_totals.
func writeCounters(ctx context.Context, tx pgx.Tx, events []Event) error {
	type counter struct {
		period, scopeType, scopeID string // period: "day", "month" or "total"
		start                      time.Time
	}
	type total struct {
		usd    float64
		tokens int64
	}
	agg := map[counter]*total{}
	for _, e := range events {
		ts := e.TS.UTC()
		tokens := int64(e.PromptTokens + e.CompletionTokens)
		for _, key := range ScopeKeys(e) {
			scopeType, scopeID, _ := strings.Cut(key, ":")
			for _, k := range []counter{
				{"day", scopeType, scopeID, ts.Truncate(24 * time.Hour)},
				{"month", scopeType, scopeID, time.Date(ts.Year(), ts.Month(), 1, 0, 0, 0, 0, time.UTC)},
				{"total", scopeType, scopeID, TotalStart},
			} {
				c := agg[k]
				if c == nil {
					c = &total{}
					agg[k] = c
				}
				c.usd += e.CostUSD
				c.tokens += tokens
			}
		}
	}
	if len(agg) == 0 {
		return nil
	}
	// Upsert in one fixed order: concurrent syncs that share counters (a
	// user's, a team's) would otherwise lock them in opposite orders and
	// deadlock.
	keys := slices.SortedFunc(maps.Keys(agg), func(a, b counter) int {
		return cmp.Or(strings.Compare(a.period, b.period), strings.Compare(a.scopeType, b.scopeType),
			strings.Compare(a.scopeID, b.scopeID), a.start.Compare(b.start))
	})
	b := &pgx.Batch{}
	for _, k := range keys {
		c := agg[k]
		if k.period == "day" {
			b.Queue(`INSERT INTO spend_counters (scope_type, scope_id, period_start, usd, tokens)
				VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (scope_type, scope_id, period_start)
				DO UPDATE SET usd = spend_counters.usd + EXCLUDED.usd,
				              tokens = spend_counters.tokens + EXCLUDED.tokens`,
				k.scopeType, k.scopeID, k.start, money(c.usd), c.tokens)
			continue
		}
		b.Queue(`INSERT INTO spend_totals (period, period_start, scope_type, scope_id, usd, tokens)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (period, period_start, scope_type, scope_id)
			DO UPDATE SET usd = spend_totals.usd + EXCLUDED.usd,
			              tokens = spend_totals.tokens + EXCLUDED.tokens`,
			k.period, k.start, k.scopeType, k.scopeID, money(c.usd), c.tokens)
	}
	return tx.SendBatch(ctx, b).Close()
}

// EnsurePartitions creates last, this and next month's usage_events
// partitions, the months in Window. Called once at startup, where a failure is fatal: without a
// partition every usage row is silently dropped while the gateway reports
// healthy. Last month is included so late gateway batches still land.
func (w *PGWriter) EnsurePartitions(ctx context.Context) error {
	return w.ensurePartitions(ctx, time.Now().UTC())
}

func (w *PGWriter) ensurePartitions(ctx context.Context, now time.Time) error {
	// One connection, so the lock and the lock timeout stay with these
	// statements. A new partition briefly locks its parent; behind a long query
	// it gives up and tries again rather than hold every insert up behind it.
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// Waited for, not tried: the other control plane's pass is short, and this
	// one must then read the bounds of what it made.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, store.LockPartitions); err != nil {
		return err
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, store.LockPartitions); err != nil {
			conn.Conn().Close(uctx)
		}
	}()
	if _, err := conn.Exec(ctx, `SET lock_timeout = '5s'`); err != nil {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `RESET lock_timeout`)
	have, err := partitionSpans(ctx, conn.Conn(), "usage_events")
	if err != nil {
		return err
	}

	lo, hi := Window(now)
	var firstErr error
	for start := lo; start.Before(hi); start = start.AddDate(0, 1, 0) {
		end := start.AddDate(0, 1, 0)
		for _, table := range []string{"usage_events", "usage_hourly", "usage_daily"} {
			name := table + "_" + start.Format("2006_01")
			from, to, with := start, end, rollupStorage
			if table == "usage_events" {
				if _, ok := have[name]; ok {
					continue
				}
				var ok bool
				if from, to, ok = eventSpan(start, have); !ok {
					continue // its neighbours cover the month already
				}
				with = ""
			}
			// DDL cannot take bind params; bounds are generated, not user input
			ddl := `CREATE TABLE IF NOT EXISTS ` + name + ` PARTITION OF ` + table +
				` FOR VALUES FROM ('` + from.Format(boundLayout) + `') TO ('` + to.Format(boundLayout) + `')` + with
			var err error
			for try := 1; ; try++ {
				if _, err = conn.Exec(ctx, ddl); err == nil || try == 3 || !store.LockTimedOut(err) {
					break
				}
			}
			if err != nil {
				w.PartitionErrors.Add(1)
				slog.Error("create partition failed", "partition", name, "err", err)
				if firstErr == nil {
					firstErr = fmt.Errorf("create partition %s: %w", name, err)
				}
				continue
			}
			if table == "usage_events" {
				have[name] = span{from, to}
			}
		}
	}
	return firstErr
}

// boundLayout writes a partition bound as a UTC instant.
const boundLayout = "2006-01-02 15:04:05Z07"

type span struct{ from, to time.Time }

// boundsRE picks the two instants out of pg_get_expr's FOR VALUES FROM (...)
// TO (...); a DEFAULT or MINVALUE partition does not match.
const boundsRE = `^FOR VALUES FROM \('([^']+)'\) TO \('([^']+)'\)$`

// partitionSpans reads the bounds of table's partitions from the catalog, by
// partition name.
func partitionSpans(ctx context.Context, conn *pgx.Conn, table string) (map[string]span, error) {
	rows, err := conn.Query(ctx, `
		SELECT c.relname, b[1]::timestamptz, b[2]::timestamptz
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid,
		     regexp_match(pg_get_expr(c.relpartbound, c.oid), $2) AS b
		WHERE i.inhparent = $1::regclass AND b IS NOT NULL`, table, boundsRE)
	if err != nil {
		return nil, err
	}
	out := map[string]span{}
	var name string
	var from, to time.Time
	_, err = pgx.ForEachRow(rows, []any{&name, &from, &to}, func() error {
		out[name] = span{from.UTC(), to.UTC()}
		return nil
	})
	return out, err
}

// eventSpan is the range a new usage_events partition for the month starting
// at start takes: that UTC month, but starting where the partition below it
// ends and ending where the one above it starts. Partitions made before 0.4
// have bounds in the database's time zone, hours off the UTC month, so the
// first new one closes the gap or overlap and those after it are UTC months.
// It returns false when nothing in the month is left uncovered.
func eventSpan(start time.Time, have map[string]span) (from, to time.Time, ok bool) {
	end := start.AddDate(0, 1, 0)
	from = start
	// No zone is more than a day off UTC: a partition ending within two days
	// before start, or inside the month, is the one below.
	found := false
	for _, s := range have {
		if s.to.After(start.Add(-48*time.Hour)) && !s.to.After(end) && (!found || s.to.After(from)) {
			from, found = s.to, true
		}
	}
	// A partition that holds from pushes it up, rather than overlap it.
	for moved := true; moved; {
		moved = false
		for _, s := range have {
			if !s.from.After(from) && s.to.After(from) {
				from, moved = s.to, true
			}
		}
	}
	to = end
	for _, s := range have {
		if !s.from.Before(from) && s.from.Before(to) {
			to = s.from
		}
	}
	return from, to, from.Before(to)
}

const rollupStorage = ` WITH (fillfactor = 90, autovacuum_vacuum_scale_factor = 0.02, autovacuum_analyze_scale_factor = 0.02)`

// maintain builds missing indexes and drops what is past its retention. An
// advisory lock keeps it to one control plane at a time.
func (w *PGWriter) maintain(ctx context.Context) {
	// The lock belongs to this connection's session; it stays idle, outside
	// any transaction, while the work runs on others.
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, store.LockMaintain).Scan(&locked); err != nil || !locked {
		return
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, store.LockMaintain); err != nil {
			conn.Conn().Close(uctx)
		}
	}()

	// Prune first: it finishes detaching partitions, which index adoption
	// would otherwise have to skip.
	if err := w.Prune(ctx, time.Now()); err != nil && ctx.Err() == nil {
		slog.Error("usage retention", "err", err)
	}
	if err := w.EnsureIndexes(ctx); err != nil && ctx.Err() == nil {
		slog.Error("usage indexes", "err", err)
	}
	for _, fn := range w.Maintenance {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			slog.Error("maintenance", "err", err)
		}
	}
	// Autovacuum analyses partitions, never the partitioned table itself, and
	// the planner needs the latter's statistics for the dashboards' queries.
	if _, err := w.pool.Exec(ctx, `ANALYZE usage_hourly, usage_daily`); err != nil && ctx.Err() == nil {
		slog.Error("analyze usage rollups", "err", err)
	}
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
