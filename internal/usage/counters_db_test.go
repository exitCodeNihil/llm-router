package usage

import (
	"context"
	"math/rand/v2"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/exitcodenihil/llm-router/internal/store"
)

// TestConcurrentSyncsDoNotDeadlock writes batches that share spend counters
// from many transactions at once, as gateways syncing together do. It needs a
// disposable database:
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/usage
func TestConcurrentSyncsDoNotDeadlock(t *testing.T) {
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
	if err := NewPGWriter(st.Pool).EnsurePartitions(ctx); err != nil {
		t.Fatal(err)
	}

	const scopes, workers, rounds, cost = 8, 8, 25, 0.01
	var keys, users, teams []string
	for range scopes {
		keys, users, teams = append(keys, uuid.NewString()), append(users, uuid.NewString()), append(teams, uuid.NewString())
	}
	batch := func() []Event {
		evs := make([]Event, scopes)
		for i := range evs {
			evs[i] = Event{TS: time.Now().UTC(), RequestID: uuid.NewString(), APIKeyID: keys[i], UserID: users[i],
				TeamID: teams[i], ModelName: "m", PromptTokens: 1, CostUSD: cost, StatusCode: 200}
		}
		rand.Shuffle(len(evs), func(i, j int) { evs[i], evs[j] = evs[j], evs[i] })
		return evs
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for range workers {
		wg.Go(func() {
			for range rounds {
				tx, err := st.Pool.Begin(ctx)
				if err != nil {
					errs <- err
					return
				}
				if _, err := InsertEvents(ctx, tx, batch()); err != nil {
					tx.Rollback(ctx)
					errs <- err
					continue
				}
				if err := tx.Commit(ctx); err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	var usd float64
	if err := st.Pool.QueryRow(ctx, `SELECT usd FROM spend_counters WHERE scope_type='key' AND scope_id=$1`, keys[0]).
		Scan(&usd); err != nil {
		t.Fatal(err)
	}
	want := workers * rounds * cost
	if usd < want-1e-9 || usd > want+1e-9 {
		t.Fatalf("key spend = %v, want %v", usd, want)
	}
	// The month and all-time rows carry the same spend as the day's.
	rows, err := st.Pool.Query(ctx, `SELECT period, usd::float8 FROM spend_totals WHERE scope_type='key' AND scope_id=$1`, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	totals := map[string]float64{}
	var period string
	if _, err := pgx.ForEachRow(rows, []any{&period, &usd}, func() error { totals[period] = usd; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, period := range []string{"month", "total"} {
		if got, ok := totals[period]; !ok || got < want-1e-9 || got > want+1e-9 {
			t.Fatalf("%s spend = %v, want %v", period, got, want)
		}
	}
}
