package usage

import (
	"context"
	"math/rand/v2"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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
	if want := workers * rounds * cost; usd < want-1e-9 || usd > want+1e-9 {
		t.Fatalf("key spend = %v, want %v", usd, want)
	}
}
