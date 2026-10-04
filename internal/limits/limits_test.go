package limits

import (
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

func TestSweepDropsVanishedScopes(t *testing.T) {
	rpm := 5
	id := &auth.Identity{Key: &snapshot.Key{ID: "k1", RPMLimit: &rpm}}
	s1 := &snapshot.Snapshot{KeysByHash: map[[32]byte]*snapshot.Key{{1}: id.Key}}
	c := New()
	c.Check(id, s1)
	if c.rpm["key:k1"] == nil {
		t.Fatal("window not created")
	}
	// The key is gone from the next snapshot (deleted, or a workspace restart
	// minted a new one): its window must go with it.
	s2 := &snapshot.Snapshot{KeysByHash: map[[32]byte]*snapshot.Key{}}
	other := &auth.Identity{Key: &snapshot.Key{ID: "k2", RPMLimit: &rpm}}
	c.Check(other, s2)
	if c.rpm["key:k1"] != nil {
		t.Fatal("window for a vanished key survived the snapshot swap")
	}
}

func TestMemberBudgetAppliesToTeamKeys(t *testing.T) {
	share := 1.0
	user := &snapshot.User{ID: "u1", Memberships: map[string]*snapshot.Membership{"t1": {BudgetUSD: &share, BudgetPeriod: "monthly"}}}
	team := &snapshot.Team{ID: "t1"}
	teamKey := &auth.Identity{Key: &snapshot.Key{ID: "k1"}, User: user, Team: team}
	s := &snapshot.Snapshot{Spend: map[string]snapshot.Spend{"member:u1:t1": {USD: 1.5}}}
	c := New()
	if ok, _, code, msg := c.Check(teamKey, s); ok || code != "budget_exceeded" || !strings.Contains(msg, "member:u1:t1") {
		t.Fatalf("member share exhausted should deny: ok=%v code=%s msg=%s", ok, code, msg)
	}
	// The same person's personal key is not the team's business.
	personal := &auth.Identity{Key: &snapshot.Key{ID: "k2"}, User: user}
	if ok, _, _, _ := c.Check(personal, s); !ok {
		t.Fatal("personal key must not be capped by a team share")
	}
	// Another member of the same team has their own share.
	other := &auth.Identity{Key: &snapshot.Key{ID: "k3"}, User: &snapshot.User{ID: "u2"}, Team: team}
	if ok, _, _, _ := c.Check(other, s); !ok {
		t.Fatal("another member is unaffected")
	}
}

func TestBudgetAndRPM(t *testing.T) {
	budget := 0.01
	rpm := 2
	id := &auth.Identity{Key: &snapshot.Key{ID: "k1", BudgetUSD: &budget, RPMLimit: &rpm}}
	s := &snapshot.Snapshot{Spend: map[string]snapshot.Spend{"key:k1": {USD: 0.005}}}
	c := New()

	// under budget, rpm slots 1 and 2 pass
	for i := 0; i < 2; i++ {
		if ok, _, code, msg := c.Check(id, s); !ok {
			t.Fatalf("request %d denied: %s %s", i+1, code, msg)
		}
	}
	// third request in the same minute breaks rpm
	if ok, _, code, _ := c.Check(id, s); ok || code != "rate_limit_exceeded" {
		t.Fatalf("expected rpm denial, got ok=%v code=%s", ok, code)
	}

	// spend past the budget via local delta
	c.RecordUsage(id, 100, 0.006) // 0.005 snapshot + 0.006 delta > 0.01
	if ok, _, code, _ := c.Check(id, s); ok || code != "budget_exceeded" {
		t.Fatalf("expected budget denial, got ok=%v code=%s", ok, code)
	}

	// a fresh snapshot (recomputed spend) resets local deltas
	s2 := &snapshot.Snapshot{Spend: map[string]snapshot.Spend{"key:k1": {USD: 0.009}}}
	if ok, _, code, _ := c.Check(id, s2); !ok && code == "budget_exceeded" {
		t.Fatal("delta should reset on snapshot swap")
	}
}

func TestTPM(t *testing.T) {
	tpm := 50
	id := &auth.Identity{User: &snapshot.User{ID: "u1", TPMLimit: &tpm}}
	s := &snapshot.Snapshot{Spend: map[string]snapshot.Spend{}}
	c := New()

	if ok, _, _, _ := c.Check(id, s); !ok {
		t.Fatal("first request should pass")
	}
	c.RecordUsage(id, 60, 0)
	if ok, _, code, _ := c.Check(id, s); ok || code != "rate_limit_exceeded" {
		t.Fatalf("expected tpm denial, got ok=%v code=%s", ok, code)
	}
}
