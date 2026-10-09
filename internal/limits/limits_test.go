package limits

import (
	"strings"
	"testing"
	"time"

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

// A burst at the end of one minute and another at the start of the next must
// not pass twice the limit: the previous minute still counts, fading out.
func TestSlidingWindowAcrossMinuteBoundary(t *testing.T) {
	rpm := 10
	id := &auth.Identity{Key: &snapshot.Key{ID: "k1", RPMLimit: &rpm}}
	s := &snapshot.Snapshot{}
	c := New()
	at := time.UnixMilli(59_000) // 1 s before the boundary
	c.now = func() time.Time { return at }
	passed := 0
	for range 20 {
		if ok, _, _, _ := c.Check(id, s); ok {
			passed++
		}
	}
	at = time.UnixMilli(61_000) // 1 s after it: the old minute still counts ~58/60
	for range 20 {
		if ok, _, _, _ := c.Check(id, s); ok {
			passed++
		}
	}
	if passed > rpm+1 {
		t.Fatalf("%d requests passed across the boundary, limit %d", passed, rpm)
	}
	at = time.UnixMilli(120_000 + 59_000) // a minute later the window has emptied
	if ok, _, _, _ := c.Check(id, s); !ok {
		t.Fatal("a request a minute later must pass")
	}
}

// Counts from gateways and from the control plane's own traffic add up.
func TestIngestCombinesGateways(t *testing.T) {
	rpm := 10
	id := &auth.Identity{Key: &snapshot.Key{ID: "k1", RPMLimit: &rpm}}
	c := New()
	at := time.UnixMilli(600_000)
	c.now = func() time.Time { return at }
	s := &snapshot.Snapshot{KeysByHash: map[[32]byte]*snapshot.Key{{1}: id.Key}}
	c.Check(id, s)               // one request through the control plane itself
	c.Ingest("key:k1", 10, 5, 0) // gateway A
	c.Ingest("key:k1", 10, 4, 0) // gateway B
	if ok, _, _, _ := c.Check(id, s); ok {
		t.Fatal("10 requests from the control plane and two gateways should exhaust a 10 rpm key")
	}
	if r := c.Export(s, "").Rates["key:k1"]; r[0] != 10 {
		t.Fatalf("exported rpm = %v, want 10 this minute", r)
	}
}

// Export sends every budgeted scope on a fresh cursor, then only what changed,
// and everything again after a snapshot swap or a control-plane restart.
func TestExportCursor(t *testing.T) {
	budget := 5.0
	k1 := &snapshot.Key{ID: "k1", BudgetUSD: &budget}
	k2 := &snapshot.Key{ID: "k2", BudgetUSD: &budget}
	free := &snapshot.Key{ID: "k3"}
	s := &snapshot.Snapshot{
		KeysByHash: map[[32]byte]*snapshot.Key{{1}: k1, {2}: k2, {3}: free},
		Spend:      map[string]snapshot.Spend{"key:k1": {USD: 1}},
	}
	c := New()
	first := c.Export(s, "")
	if len(first.Spend) != 2 || first.Spend["key:k1"] != 1 || first.Spend["key:k2"] != 0 {
		t.Fatalf("first export = %v, want both budgeted keys", first.Spend)
	}
	if again := c.Export(s, first.Cursor); len(again.Spend) != 0 {
		t.Fatalf("nothing changed, got %v", again.Spend)
	}
	c.AddSpend("key:k1", 0.5)
	if next := c.Export(s, first.Cursor); len(next.Spend) != 1 || next.Spend["key:k1"] != 1.5 {
		t.Fatalf("after spend, got %v", next.Spend)
	}
	s2 := &snapshot.Snapshot{KeysByHash: s.KeysByHash, Spend: map[string]snapshot.Spend{"key:k1": {USD: 1.5}}}
	if swapped := c.Export(s2, first.Cursor); len(swapped.Spend) != 2 || swapped.Spend["key:k1"] != 1.5 {
		t.Fatalf("after a snapshot swap, got %v", swapped.Spend)
	}
	if other := New().Export(s, first.Cursor); len(other.Spend) != 2 {
		t.Fatalf("a cursor from another process must get everything, got %v", other.Spend)
	}
}
