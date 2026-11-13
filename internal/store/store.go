// Package store owns the Postgres connection pool and schema migrations.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	// Pool serves ingest, gateway sync, snapshots and writes.
	Pool *pgxpool.Pool
	// Console serves console reads, under a statement timeout, so a slow
	// dashboard query can neither hold ingest's connections nor run forever.
	Console *pgxpool.Pool
}

// Options sizes the pools. Zero values take the defaults.
type Options struct {
	MaxConns        int           // Pool; default 16
	ConsoleMaxConns int           // Console; default 8
	ConsoleTimeout  time.Duration // statement_timeout on Console; default 30s
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	return OpenWith(ctx, databaseURL, Options{})
}

func OpenWith(ctx context.Context, databaseURL string, o Options) (*Store, error) {
	if o.MaxConns <= 0 {
		o.MaxConns = 16
	}
	if o.ConsoleMaxConns <= 0 {
		o.ConsoleMaxConns = 8
	}
	if o.ConsoleTimeout <= 0 {
		o.ConsoleTimeout = 30 * time.Second
	}
	pool, err := newPool(ctx, databaseURL, o.MaxConns, 0)
	if err != nil {
		return nil, err
	}
	console, err := newPool(ctx, databaseURL, o.ConsoleMaxConns, o.ConsoleTimeout)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool, Console: console}, nil
}

func newPool(ctx context.Context, databaseURL string, maxConns int, timeout time.Duration) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	cfg.MaxConns = int32(maxConns)
	// A transaction left open and idle holds back vacuum for every table.
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "60000"
	if timeout > 0 {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(timeout.Milliseconds(), 10)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

func (s *Store) Close() {
	s.Pool.Close()
	if s.Console != nil {
		s.Console.Close()
	}
}

// Migrate applies embedded migrations in filename order, forward-only.
// It holds an advisory lock throughout, so two control planes starting
// together apply each migration once: the second waits, then finds them done.
// ponytail: swap for pressly/goose if down-migrations are ever needed.
func (s *Store) Migrate(ctx context.Context) error {
	// One connection: the lock belongs to its session.
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, LockMigrate); err != nil {
		return err
	}
	defer func() {
		// Even when ctx is done: a connection returned to the pool still
		// holding the lock would block every later start.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, LockMigrate); err != nil {
			conn.Conn().Close(uctx) // closing the session releases it
		}
	}()

	if _, err := conn.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		var applied bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		sql, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		slog.Info("applying migration", "name", name)
		// A migration that cannot get its table lock gives up rather than
		// queue: every query on that table would queue behind it.
		for try := 1; ; try++ {
			err = migrateOne(ctx, conn, name, string(sql))
			if err == nil || try == 3 || !LockTimedOut(err) {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func migrateOne(ctx context.Context, conn *pgxpool.Conn, name, sql string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// UTC whatever the database's own zone: migrations do date arithmetic,
	// and month boundaries must not move with it.
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '10s'; SET LOCAL TimeZone = 'UTC'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LockTimedOut reports whether err is Postgres giving up on a lock
// (lock_timeout): worth another try.
func LockTimedOut(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "55P03"
}

// BumpConfigVersion increments the config version counter and NOTIFYs listeners.
// Called by every mgmt-API write so gateways rebuild their snapshot.
func (s *Store) BumpConfigVersion(ctx context.Context) error {
	if _, err := s.Pool.Exec(ctx,
		`UPDATE settings SET value = to_jsonb((value::text)::bigint + 1) WHERE key = 'config_version'`); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `NOTIFY llmr_config`)
	return err
}
