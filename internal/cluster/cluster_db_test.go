package cluster

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/limits"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
)

// ledger stands in for Postgres' spend totals.
type ledger struct {
	mu  sync.Mutex
	usd map[string]float64
}

func (l *ledger) add(scope string, usd float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.usd[scope] += usd
}

func (l *ledger) read(_ context.Context, scopes []string) (map[string]float64, time.Time, error) {
	at := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]float64{}
	for _, s := range scopes {
		out[s] = l.usd[s]
	}
	return out, at, nil
}

// Two control planes on one database: a rate limit and a budget hold across
// both, spend is never counted twice, and a replica that joins late hears
// what was counted before it came.
func TestTwoReplicasShareLimits(t *testing.T) {
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
	st.Pool.Exec(ctx, `TRUNCATE cp_rate_log, cp_replicas`)

	rpm, budget := 4, 1.0
	k := &snapshot.Key{ID: "k1", RPMLimit: &rpm, BudgetUSD: &budget}
	snap := &snapshot.Snapshot{KeysByHash: map[[32]byte]*snapshot.Key{{1}: k},
		Spend: map[string]snapshot.Spend{}, SpendAt: time.Now().Add(-time.Minute)}
	id := &auth.Identity{Key: k}
	led := &ledger{usd: map[string]float64{}}
	node := func() (*Node, *limits.Checker) {
		c := limits.New()
		c.Observe(snap)
		return &Node{Pool: st.Pool, Limits: c, Spend: led.read, Version: "test"}, c
	}
	a, ca := node()

	// Alone, a replica writes nothing.
	ca.Check(id, snap)
	a.Tick(ctx)
	var n int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM cp_rate_log`).Scan(&n)
	if n != 0 || len(a.Peers()) != 0 {
		t.Fatalf("a lone replica wrote %d rows, sees %d peers", n, len(a.Peers()))
	}

	// b joins: it hears the request a took before it came.
	b, cb := node()
	b.Tick(ctx)
	a.Tick(ctx) // a sees b, hands over its counts so far
	b.Tick(ctx)
	if len(a.Peers()) != 1 || len(b.Peers()) != 1 {
		t.Fatalf("peers: a %d, b %d", len(a.Peers()), len(b.Peers()))
	}
	for i := range 2 {
		if ok, _, code, _ := cb.Check(id, snap); !ok {
			t.Fatalf("b refused request %d: %s", i+2, code)
		}
	}
	b.Tick(ctx)
	a.Tick(ctx)
	if ok, _, _, _ := ca.Check(id, snap); !ok { // the 4th
		t.Fatal("a refused the 4th request of 4 per minute")
	}
	a.Tick(ctx)
	b.Tick(ctx)
	for name, c := range map[string]*limits.Checker{"a": ca, "b": cb} {
		if ok, _, code, _ := c.Check(id, snap); ok || code != "rate_limit_exceeded" {
			t.Fatalf("%s admitted a 5th request in the minute", name)
		}
	}
	// Reading the same rows again adds nothing.
	a.Tick(ctx)
	b.Tick(ctx)
	if got := ca.Export(snap, "").Rates["key:k1"][0]; got != 4 {
		t.Fatalf("a counts %d requests, want 4", got)
	}

	// Spend: a's request costs 0.6 and is committed; b must see it without a
	// new snapshot, and a must not count it twice.
	ca.RecordUsage(id, 0, 0.6)
	led.add("key:k1", 0.6)
	ca.Committed("key:k1", 0.6)
	a.Tick(ctx)
	b.Tick(ctx)
	for name, c := range map[string]*limits.Checker{"a": ca, "b": cb} {
		if got := c.Export(snap, "").Spend["key:k1"]; got != 0.6 {
			t.Fatalf("%s sees spend %v, want 0.6", name, got)
		}
	}
	cb.AddSpend("key:k1", 0.5) // a gateway of b's, already committed
	led.add("key:k1", 0.5)
	b.Tick(ctx)
	a.Tick(ctx)
	if got := ca.Export(snap, "").Spend["key:k1"]; got != 1.1 {
		t.Fatalf("a sees spend %v, want 1.1", got)
	}
	if got := cb.Export(snap, "").Spend["key:k1"]; got != 1.1 {
		t.Fatalf("b sees spend %v, want 1.1", got)
	}

	// b leaves: a stops writing.
	st.Pool.Exec(ctx, `DELETE FROM cp_replicas WHERE id = $1`, b.ID())
	a.Tick(ctx)
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM cp_rate_log WHERE replica = $1`, a.ID()).Scan(&n)
	ca.Check(&auth.Identity{Key: &snapshot.Key{ID: "k2", RPMLimit: &rpm}}, snap)
	a.Tick(ctx)
	var after int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM cp_rate_log WHERE replica = $1`, a.ID()).Scan(&after)
	if after != n {
		t.Fatalf("a wrote %d rows with no peer", after-n)
	}
}

// A replica that is not seen for a while (a stall, a database blip) and comes
// back is neither read from the start again nor sent what it already has.
func TestPeerDroppingOutAndBack(t *testing.T) {
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
	st.Pool.Exec(ctx, `TRUNCATE cp_rate_log, cp_replicas`)
	rpm := 1000
	k := &snapshot.Key{ID: "k1", RPMLimit: &rpm}
	snap := &snapshot.Snapshot{KeysByHash: map[[32]byte]*snapshot.Key{{1}: k}, Spend: map[string]snapshot.Spend{}}
	id := &auth.Identity{Key: k}
	node := func() (*Node, *limits.Checker) {
		c := limits.New()
		c.Observe(snap)
		return &Node{Pool: st.Pool, Limits: c, Version: "test"}, c
	}
	a, ca := node()
	b, cb := node()
	a.Tick(ctx)
	b.Tick(ctx)
	a.Tick(ctx)
	for i := 0; i < 10; i++ {
		ca.Check(id, snap)
		cb.Check(id, snap)
	}
	for i := 0; i < 3; i++ {
		a.Tick(ctx)
		b.Tick(ctx)
	}
	sum := func(c *limits.Checker) int64 { r := c.Export(snap, "").Rates["key:k1"]; return r[0] + r[1] }
	t.Logf("before flap: a=%d b=%d (real 20)", sum(ca), sum(cb))
	// b is not seen for 11 s (a stall, or its connection to Postgres), then returns.
	st.Pool.Exec(ctx, `UPDATE cp_replicas SET last_seen = now() - interval '11 seconds' WHERE id = $1`, b.ID())
	a.Tick(ctx)
	for i := 0; i < 3; i++ {
		b.Tick(ctx)
		a.Tick(ctx)
	}
	t.Logf("after flap: a=%d b=%d (real 20)", sum(ca), sum(cb))
	if sum(ca) != 20 || sum(cb) != 20 {
		t.Fatal("counts multiplied")
	}
}
