package store

import (
	"context"
	"math/rand/v2"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
)

// Two control planes starting together on an empty database apply each
// migration once. It needs a server it may create a database on:
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/store
func TestConcurrentMigrate(t *testing.T) {
	base := os.Getenv("LLMR_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("LLMR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := Open(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "llmr_migrate_" + strconv.Itoa(rand.IntN(1e9))
	if _, err := admin.Pool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	t.Cleanup(func() { admin.Pool.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`) })

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			st, err := Open(ctx, u.String())
			if err != nil {
				errs[i] = err
				return
			}
			defer st.Close()
			errs[i] = st.Migrate(ctx)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	st, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	files, _ := migrationsFS.ReadDir("migrations")
	var applied, held int
	if err := st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM schema_migrations),
		(SELECT count(*) FROM pg_locks WHERE locktype = 'advisory')`).Scan(&applied, &held); err != nil {
		t.Fatal(err)
	}
	if applied != len(files) {
		t.Fatalf("%d migrations recorded, want %d", applied, len(files))
	}
	if held != 0 {
		t.Fatalf("%d advisory locks still held after Migrate", held)
	}
	// The storage settings reach the partitions created by the migrations.
	var tuned int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent IN ('usage_hourly'::regclass, 'usage_daily'::regclass)
		  AND c.reloptions @> ARRAY['fillfactor=90']`).Scan(&tuned); err != nil {
		t.Fatal(err)
	}
	if tuned == 0 {
		t.Fatal("no rollup partition carries the storage settings")
	}
}
