package usage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Retention is how long usage is kept: that many whole months before the
// current one. Zero keeps it for ever.
type Retention struct {
	// RawMonths is for usage_events, the per-request rows. At least 2, so the
	// months late gateway batches may still land in (Window) are never dropped.
	RawMonths int
	// RollupMonths is for the daily rollups and per-day spend. At least 3.
	RollupMonths int
}

func (r Retention) Validate() error {
	if r.RawMonths != 0 && r.RawMonths < 2 {
		return fmt.Errorf("usage retention must be 0 (keep for ever) or at least 2 months")
	}
	if r.RollupMonths != 0 && r.RollupMonths < 3 {
		return fmt.Errorf("rollup retention must be 0 (keep for ever) or at least 3 months")
	}
	return nil
}

// Prune drops what is older than the retention: whole monthly partitions of
// usage_events and the rollups, and old per-day spend. Hourly rollups are
// always kept three months (HourlyFrom). It must not run inside a transaction.
func (w *PGWriter) Prune(ctx context.Context, now time.Time) error {
	now = now.UTC()
	cur := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	// One connection, so the lock timeout below stays with these statements.
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// Give up on a lock held by a long query; the next pass tries again.
	if _, err := conn.Exec(ctx, `SET lock_timeout = '10s'`); err != nil {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `RESET lock_timeout`)

	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if r := w.Retention.RawMonths; r > 0 {
		keep(w.dropPartitions(ctx, conn.Conn(), "usage_events", cur.AddDate(0, -r, 0)))
	}
	keep(w.dropPartitions(ctx, conn.Conn(), "usage_hourly", HourlyFrom(now)))
	if r := w.Retention.RollupMonths; r > 0 {
		before := cur.AddDate(0, -r, 0)
		keep(w.dropPartitions(ctx, conn.Conn(), "usage_daily", before))
		// A scope's all-time row in spend_totals stays: total budgets read it.
		if _, err := conn.Exec(ctx, `DELETE FROM spend_counters WHERE period_start < $1`, before); err != nil {
			keep(fmt.Errorf("prune spend_counters: %w", err))
		}
		if _, err := conn.Exec(ctx, `DELETE FROM spend_totals WHERE period = 'month' AND period_start < $1`, before); err != nil {
			keep(fmt.Errorf("prune spend_totals: %w", err))
		}
	}
	return firstErr
}

// dropPartitions drops table's monthly partitions (table_YYYY_MM) that start
// before the given month. A partition is detached without blocking writes to
// the others, then dropped; one left half-detached or detached by an earlier,
// interrupted pass is finished off.
func (w *PGWriter) dropPartitions(ctx context.Context, conn *pgx.Conn, table string, before time.Time) error {
	rows, err := conn.Query(ctx, `
		SELECT c.relname, i.inhrelid IS NOT NULL, coalesce(i.inhdetachpending, false), c.reltuples::bigint
		FROM pg_class c
		LEFT JOIN pg_inherits i ON i.inhrelid = c.oid AND i.inhparent = $1::regclass
		WHERE c.relkind = 'r' AND c.relname ~ ('^' || $1 || '_[0-9]{4}_[0-9]{2}$')
		  AND c.relnamespace = (SELECT relnamespace FROM pg_class WHERE oid = $1::regclass)
		ORDER BY 1`, table)
	if err != nil {
		return err
	}
	type part struct {
		name              string
		attached, pending bool
		rows              int64
	}
	parts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (part, error) {
		var p part
		return p, r.Scan(&p.name, &p.attached, &p.pending, &p.rows)
	})
	if err != nil {
		return err
	}
	var firstErr error
	for _, p := range parts {
		month, err := time.Parse("2006_01", p.name[len(table)+1:])
		if err != nil || !month.Before(before) {
			continue
		}
		// names come from the catalog and match the pattern above
		if p.attached {
			how := "CONCURRENTLY"
			if p.pending {
				how = "FINALIZE"
			}
			if _, err := conn.Exec(ctx, `ALTER TABLE `+table+` DETACH PARTITION `+p.name+` `+how); err != nil {
				slog.Error("detach partition failed", "partition", p.name, "err", err)
				if firstErr == nil {
					firstErr = fmt.Errorf("detach %s: %w", p.name, err)
				}
				continue
			}
		}
		if _, err := conn.Exec(ctx, `DROP TABLE `+p.name); err != nil {
			slog.Error("drop partition failed", "partition", p.name, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("drop %s: %w", p.name, err)
			}
			continue
		}
		w.PartitionsDropped.Add(1)
		slog.Info("dropped usage partition past its retention", "partition", p.name, "rows_estimate", max(p.rows, 0))
	}
	return firstErr
}
