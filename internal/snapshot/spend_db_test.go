package snapshot

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/store"
)

// loadSpend gives each budgeted scope the spend of its own period, and reads
// nothing for a scope without a budget. It needs a disposable database:
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/snapshot
func TestLoadSpendByPeriod(t *testing.T) {
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

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	// Every scope has spent 1 today, 5 this month and 20 in all.
	spent := func(scopeType, id string) {
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO spend_counters (scope_type, scope_id, period_start, usd) VALUES ($1,$2,$3,1)`, []any{scopeType, id, today}},
			{`INSERT INTO spend_counters (scope_type, scope_id, period_start, usd) VALUES ($1,$2,$3,19)`, []any{scopeType, id, today.AddDate(0, -2, 0)}},
			{`INSERT INTO spend_totals (period, period_start, scope_type, scope_id, usd) VALUES ('month',$3,$1,$2,5)`, []any{scopeType, id, month}},
			{`INSERT INTO spend_totals (period, period_start, scope_type, scope_id, usd) VALUES ('month',$3,$1,$2,9)`, []any{scopeType, id, month.AddDate(0, -1, 0)}},
			{`INSERT INTO spend_totals (period, period_start, scope_type, scope_id, usd) VALUES ('total',$3,$1,$2,20)`, []any{scopeType, id, spendTotalStart}},
		} {
			if _, err := st.Pool.Exec(ctx, q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	budget := 100.0
	keyDaily, keyNone, user, team, member := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	s := &Snapshot{
		KeysByHash: map[[32]byte]*Key{
			{1}: {ID: keyDaily, BudgetUSD: &budget, BudgetPeriod: "daily"},
			{2}: {ID: keyNone},
		},
		UsersByID: map[string]*User{
			user: {ID: user, BudgetUSD: &budget, BudgetPeriod: "monthly"},
			member: {ID: member, Memberships: map[string]*Membership{
				team: {BudgetUSD: &budget, BudgetPeriod: "total"},
			}},
		},
		TeamsByID: map[string]*Team{team: {ID: team, BudgetUSD: &budget}}, // no period: all time
		Spend:     map[string]Spend{},
	}
	for scopeType, id := range map[string]string{"key": keyDaily, "user": user, "team": team, "member": member + ":" + team} {
		spent(scopeType, id)
	}
	spent("key", keyNone)

	if err := (&Builder{Pool: st.Pool}).loadSpend(ctx, s); err != nil {
		t.Fatal(err)
	}
	want := map[string]Spend{
		"key:" + keyDaily:               {USD: 1},
		"user:" + user:                  {USD: 5},
		"team:" + team:                  {USD: 20},
		"member:" + member + ":" + team: {USD: 20},
	}
	if len(s.Spend) != len(want) {
		t.Fatalf("spend = %v, want %v", s.Spend, want)
	}
	for k, w := range want {
		if s.Spend[k] != w {
			t.Fatalf("%s = %v, want %v", k, s.Spend[k], w)
		}
	}
}
