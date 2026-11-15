package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/exitcodenihil/llm-router/internal/store"
)

// eventIndexes are usage_events' indexes besides its primary key: the
// partitioned table's name for each, the name 0.3 gave each partition's copy
// (partition name and suffix), and the columns.
var eventIndexes = []struct{ name, suffix, cols string }{
	{"usage_events_team_ts", "_team_ts", "(team_id, ts)"},
	{"usage_events_key_ts", "_key_ts", "(api_key_id, ts)"},
	{"usage_events_user_ts", "_user_ts", "(user_id, ts)"},
	// finding one request by its id
	{"usage_events_request", "_request", "(request_id)"},
}

// EnsureIndexes gives usage_events its indexes as indexes of the partitioned
// table, so a partition gets them as it is created. For the partitions that
// exist, each partition's index is built CONCURRENTLY, so writes carry on
// while a large one is indexed, and then attached; one 0.3 built is reused.
// It also moves the primary key from (id, ts) to (ts, id), which serves the
// Requests feed, newest first, in place of the (ts DESC) indexes 0.3 added to
// every partition. It must not run inside a transaction.
func (w *PGWriter) EnsureIndexes(ctx context.Context) error {
	if !w.indexing.TryLock() {
		return nil // a pass is already running
	}
	defer w.indexing.Unlock()
	// One connection, so the lock timeout stays with these statements.
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	c := conn.Conn()
	if _, err := c.Exec(ctx, `SET lock_timeout = '5s'`); err != nil {
		return err
	}
	defer c.Exec(context.WithoutCancel(ctx), `RESET lock_timeout`)
	parts, err := eventPartitions(ctx, c)
	if err != nil {
		return err
	}
	var firstErr error
	for _, idx := range eventIndexes {
		if err := adoptIndex(ctx, c, idx.name, idx.suffix, idx.cols, parts); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := adoptKey(ctx, c, parts); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

type eventPartition struct {
	name    string
	pending bool // being detached
}

func eventPartitions(ctx context.Context, c *pgx.Conn) ([]eventPartition, error) {
	rows, err := c.Query(ctx, `SELECT c.relname, i.inhdetachpending FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'usage_events'::regclass ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (eventPartition, error) {
		var p eventPartition
		return p, r.Scan(&p.name, &p.pending)
	})
}

// adoptIndex creates the partitioned table's index name, then gives each
// partition that lacks one its own and attaches it. The partitioned table's
// index is valid once every partition's is attached.
func adoptIndex(ctx context.Context, c *pgx.Conn, name, suffix, cols string, parts []eventPartition) error {
	// ON ONLY: on the partitioned table alone, which builds nothing.
	// names are generated, not user input
	if err := locking(ctx, c, `CREATE INDEX IF NOT EXISTS `+name+` ON ONLY usage_events `+cols); err != nil {
		return fmt.Errorf("create index %s: %w", name, err)
	}
	var firstErr error
	for _, p := range parts {
		if p.pending {
			continue
		}
		// What is attached, never a name: a partition created after the
		// index got its copy with it, under a name Postgres chose.
		var attached bool
		if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_inherits h JOIN pg_index i ON i.indexrelid = h.inhrelid
			WHERE h.inhparent = $1::regclass AND i.indrelid = $2::regclass)`, name, p.name).Scan(&attached); err != nil {
			return err
		}
		if attached {
			continue
		}
		child := p.name + suffix
		err := build(ctx, c, child, p.name, `CREATE INDEX CONCURRENTLY IF NOT EXISTS `+child+` ON `+p.name+` `+cols)
		if err == nil {
			err = locking(ctx, c, `ALTER INDEX `+name+` ATTACH PARTITION `+child)
		}
		if err != nil {
			slog.Error("usage partition index failed", "index", child, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("index %s: %w", child, err)
			}
		}
	}
	return firstErr
}

// adoptKey moves the primary key to (ts, id). Each partition's new key index
// is built concurrently first; the swap is one transaction that only changes
// the catalog, so it holds its locks briefly.
func adoptKey(ctx context.Context, c *pgx.Conn, parts []eventPartition) error {
	var pk, cols string
	err := c.QueryRow(ctx, `
		SELECT c.conname, array_to_string(array(
		    SELECT a.attname FROM unnest(c.conkey) WITH ORDINALITY k(n, o)
		    JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.n ORDER BY k.o), ',')
		FROM pg_constraint c WHERE c.conrelid = 'usage_events'::regclass AND c.contype = 'p'`).Scan(&pk, &cols)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if cols != "ts,id" {
		for _, p := range parts {
			if p.pending {
				return nil // the swap takes every partition; Prune finishes the detach first
			}
		}
		for _, p := range parts {
			idx := p.name + "_ts_id"
			if err := build(ctx, c, idx, p.name, `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS `+idx+` ON `+p.name+` (ts, id)`); err != nil {
				return fmt.Errorf("index %s: %w", idx, err)
			}
		}
		for try := 1; ; try++ {
			if err = swapKey(ctx, c, pk); err == nil || try == 3 || !store.LockTimedOut(err) {
				break
			}
		}
		if errors.Is(err, errNotReady) {
			return nil // a partition came or went meanwhile: the next pass
		}
		if err != nil {
			return fmt.Errorf("primary key (ts, id): %w", err)
		}
		slog.Info("usage_events primary key is now (ts, id)")
	}
	// The key serves what the (ts DESC) indexes did.
	var firstErr error
	for _, p := range parts {
		idx := p.name + "_ts"
		var old bool
		if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_index i WHERE i.indexrelid = to_regclass($1)
			AND i.indrelid = $2::regclass AND NOT i.indisprimary
			AND NOT EXISTS (SELECT 1 FROM pg_inherits h WHERE h.inhrelid = i.indexrelid))`, idx, p.name).Scan(&old); err != nil {
			return err
		}
		if !old {
			continue
		}
		if err := concurrently(ctx, c, `DROP INDEX CONCURRENTLY IF EXISTS `+idx); err != nil {
			slog.Error("drop partition index failed", "index", idx, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("drop index %s: %w", idx, err)
			}
		}
	}
	return firstErr
}

var errNotReady = errors.New("a partition has no (ts, id) index yet")

// swapKey replaces the primary key pk ("" for none) with (ts, id), using each
// partition's (ts, id) index.
func swapKey(ctx context.Context, c *pgx.Conn, pk string) error {
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	// First, for its lock: no partition is created or detached while the list
	// below is read and used.
	if pk != "" {
		if _, err := tx.Exec(ctx, `ALTER TABLE usage_events DROP CONSTRAINT `+pk); err != nil {
			return err
		}
	} else if _, err := tx.Exec(ctx, `LOCK TABLE usage_events IN ACCESS EXCLUSIVE MODE`); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `
		SELECT c.relname, NOT i.inhdetachpending AND coalesce((SELECT x.indisvalid FROM pg_index x
		    WHERE x.indexrelid = to_regclass(c.relname || '_ts_id') AND x.indrelid = c.oid), false)
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'usage_events'::regclass ORDER BY 1`)
	if err != nil {
		return err
	}
	var parts []string
	var name string
	var ready bool
	if _, err := pgx.ForEachRow(rows, []any{&name, &ready}, func() error {
		if !ready {
			return errNotReady
		}
		parts = append(parts, name)
		return nil
	}); err != nil {
		return err
	}
	// ONLY: the partitioned table's index alone, valid once each partition's
	// key is attached below.
	if _, err := tx.Exec(ctx, `ALTER TABLE ONLY usage_events ADD CONSTRAINT usage_events_pkey PRIMARY KEY (ts, id)`); err != nil {
		return err
	}
	for _, p := range parts {
		// USING INDEX renames p_ts_id to p_pkey.
		if _, err := tx.Exec(ctx, `ALTER TABLE `+p+` ADD CONSTRAINT `+p+`_pkey PRIMARY KEY USING INDEX `+p+`_ts_id`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `ALTER INDEX usage_events_pkey ATTACH PARTITION `+p+`_pkey`); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// locking runs a statement that takes a lock writers would queue behind: it
// gives up after the lock timeout, and is tried three times.
func locking(ctx context.Context, c *pgx.Conn, sql string) error {
	for try := 1; ; try++ {
		_, err := c.Exec(ctx, sql)
		if err == nil || try == 3 || !store.LockTimedOut(err) {
			return err
		}
	}
}

// concurrently runs CREATE or DROP INDEX CONCURRENTLY with no lock timeout.
// Neither holds a lock writers wait for, while a timeout on the wait for older
// transactions at the end would throw away the build of a whole partition.
func concurrently(ctx context.Context, c *pgx.Conn, sql string) error {
	if _, err := c.Exec(ctx, `SET lock_timeout = 0`); err != nil {
		return err
	}
	_, err := c.Exec(ctx, sql)
	if _, rerr := c.Exec(context.WithoutCancel(ctx), `SET lock_timeout = '5s'`); err == nil {
		err = rerr
	}
	return err
}

// build creates the index name on table with ddl unless a valid one is there.
// An interrupted build leaves an invalid index, which is dropped first.
func build(ctx context.Context, c *pgx.Conn, name, table, ddl string) error {
	var valid *bool
	if err := c.QueryRow(ctx, `SELECT (SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass($1) AND indrelid = $2::regclass)`,
		name, table).Scan(&valid); err != nil {
		return err
	}
	if valid != nil && *valid {
		return nil
	}
	if valid != nil {
		if err := concurrently(ctx, c, `DROP INDEX CONCURRENTLY IF EXISTS `+name); err != nil {
			return err
		}
	}
	return concurrently(ctx, c, ddl)
}
