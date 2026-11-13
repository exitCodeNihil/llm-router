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
	part := "usage_events_" + month.Format("2006_01")
	t.Cleanup(func() {
		cancel()
		<-done
		st.Pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+part)
	})

	id := uuid.NewString()
	w.Write(Event{TS: month.AddDate(0, 0, 9), RequestID: id, ModelName: "m", StatusCode: 200})
	eventually(t, "a failed flush", func() bool { return w.FlushFailures.Load() > 0 })

	if _, err := st.Pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s PARTITION OF usage_events FOR VALUES FROM ('%s') TO ('%s')`,
		part, month.Format("2006-01-02"), month.AddDate(0, 1, 0).Format("2006-01-02"))); err != nil {
		t.Fatal(err)
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
