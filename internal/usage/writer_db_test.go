package usage

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("LLMR_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LLMR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A flush that fails keeps its events and writes them once Postgres takes
// them. Here the failure is an event older than any partition; the write
// succeeds once its partition exists.
func TestFlushRetriesUntilWritten(t *testing.T) {
	st := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	w := NewPGWriter(st.Pool)
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	month := time.Date(1000+rand.IntN(900), 3, 1, 0, 0, 0, 0, time.UTC)
	suffix := "_" + month.Format("2006_01")
	tables := []string{"usage_events", "usage_hourly", "usage_daily"}
	t.Cleanup(func() {
		cancel()
		<-done
		for _, table := range tables {
			st.Pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+table+suffix)
		}
	})

	id := uuid.NewString()
	w.Write(Event{TS: month.AddDate(0, 0, 9), RequestID: id, ModelName: "m", StatusCode: 200})
	eventually(t, "a failed flush", func() bool { return w.FlushFailures.Load() > 0 })

	for _, table := range tables {
		if _, err := st.Pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
			table+suffix, table, month.Format("2006-01-02Z07"), month.AddDate(0, 1, 0).Format("2006-01-02Z07"))); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the event to be written", func() bool {
		var n int
		st.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE request_id = $1`, id).Scan(&n)
		return n == 1
	})
	if w.Lost.Load() != 0 || w.Dropped.Load() != 0 {
		t.Fatalf("lost %d, dropped %d, want none", w.Lost.Load(), w.Dropped.Load())
	}
}

// Every partition gets every index, and an invalid one left by an interrupted
// build is replaced.
func TestEnsureIndexesCoversEveryPartition(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	w := NewPGWriter(st.Pool)
	if err := w.EnsurePartitions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	var missing int
	if err := st.Pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid,
		       unnest($1::text[]) AS suffix
		WHERE i.inhparent = 'usage_events'::regclass
		  AND NOT coalesce((SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass(c.relname || suffix)), false)`,
		[]string{"_team_ts", "_key_ts", "_user_ts", "_ts", "_request"}).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("%d partition indexes missing or invalid", missing)
	}
}

// Prune drops the months past the retention and nothing newer: not last
// month, which late gateway batches still write to.
func TestPruneDropsOnlyOldMonths(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	w := NewPGWriter(st.Pool)
	if err := w.EnsurePartitions(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cur := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	old := cur.AddDate(0, -7, 0)
	tables := []string{"usage_events", "usage_hourly", "usage_daily"}
	exists := func(name string) bool {
		var ok bool
		if err := st.Pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	makeOld := func() {
		for _, table := range tables {
			if _, err := st.Pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
				table+"_"+old.Format("2006_01"), table, old.Format("2006-01-02Z07"), old.AddDate(0, 1, 0).Format("2006-01-02Z07"))); err != nil {
				t.Fatal(err)
			}
		}
	}
	scope := uuid.NewString()
	for _, day := range []time.Time{old, cur} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO spend_counters (scope_type, scope_id, period_start, usd) VALUES ('key',$1,$2,1)`, scope, day); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO spend_totals (period, period_start, scope_type, scope_id, usd)
		VALUES ('month',$2,'key',$1,1), ('total',$3,'key',$1,2)`, scope, old, TotalStart); err != nil {
		t.Fatal(err)
	}

	// With no retention set only the hourly rollups age out.
	makeOld()
	if err := w.Prune(ctx, now); err != nil {
		t.Fatal(err)
	}
	suffix := "_" + old.Format("2006_01")
	if exists("usage_hourly"+suffix) || !exists("usage_events"+suffix) || !exists("usage_daily"+suffix) {
		t.Fatal("without a retention, only the old hourly partition should go")
	}

	makeOld()
	w.Retention = Retention{RawMonths: 2, RollupMonths: 3}
	if err := w.Prune(ctx, now); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		if exists(table + suffix) {
			t.Errorf("%s%s is past its retention and still there", table, suffix)
		}
		for _, month := range []time.Time{cur.AddDate(0, -1, 0), cur} {
			if name := table + "_" + month.Format("2006_01"); !exists(name) {
				t.Errorf("%s was dropped", name)
			}
		}
	}
	var days, months int
	var total float64
	if err := st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM spend_counters WHERE scope_id=$1),
		(SELECT count(*) FROM spend_totals WHERE scope_id=$1 AND period='month'),
		(SELECT usd::float8 FROM spend_totals WHERE scope_id=$1 AND period='total')`, scope).Scan(&days, &months, &total); err != nil {
		t.Fatal(err)
	}
	if days != 1 || months != 0 || total != 2 {
		t.Fatalf("spend left: %d days, %d months, total %v; want 1, 0, 2", days, months, total)
	}
	if w.PartitionsDropped.Load() != 4 {
		t.Fatalf("dropped %d partitions, want 4", w.PartitionsDropped.Load())
	}
}

// The histogram ranges in Go and in llmr_hist are the same.
func TestHistogramMatchesSQL(t *testing.T) {
	st := testStore(t)
	for _, ms := range []int{0, 9, 10, 24, 25, 999, 1000, 1001, 74999, 75000, 499999, 500000, 9000000} {
		var hist []int64
		if err := st.Pool.QueryRow(context.Background(), `SELECT llmr_hist($1)`, ms).Scan(&hist); err != nil {
			t.Fatal(err)
		}
		if len(hist) != len(histBounds)+1 || hist[histBucket(ms)] != 1 {
			t.Errorf("llmr_hist(%d) = %v, want a 1 at %d of %d", ms, hist, histBucket(ms), len(histBounds)+1)
		}
	}
}
