package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/provider"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// A failing primary is skipped on the next request while it cools, and the
// backup serves first try — that is the whole point of the breaker.
func TestCooldownSkipsFailedDeployment(t *testing.T) {
	var badHits int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&badHits, 1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer good.Close()

	g, _, key := newTestServer(t, map[string][]*snapshot.Deployment{
		"m": {stubDeployment(bad.URL, "m", "primary", 0), stubDeployment(good.URL, "m", "backup", 1)},
	})
	upstreamHealth.ok("dep-primary")

	for i := 0; i < 3; i++ {
		rec := doReq(t, g, key, `{"model":"m","messages":[]}`)
		if rec.Code != 200 {
			t.Fatalf("request %d: status %d", i, rec.Code)
		}
	}
	if n := atomic.LoadInt32(&badHits); n != 1 {
		t.Fatalf("primary was hit %d times; the cooldown should have held it out after the first", n)
	}
	cooling := CoolingDeployments()
	if len(cooling) != 1 || cooling[0].DeploymentID != "dep-primary" || cooling[0].Failures != 1 {
		t.Fatalf("cooling = %+v", cooling)
	}
	// Retry-After beats the 30s base backoff.
	if until := time.Until(cooling[0].Until); until < 100*time.Second {
		t.Errorf("Retry-After ignored: cooldown only %s", until)
	}
	upstreamHealth.ok("dep-primary")
}

func TestCooldownBackoffAndReset(t *testing.T) {
	h := &health{state: map[string]upstreamState{}}
	now := time.Now()
	h.fail("d", 0, now)
	if h.state["d"].Until.Sub(now) != cooldownBase {
		t.Errorf("first failure: %s", h.state["d"].Until.Sub(now))
	}
	h.fail("d", 0, now)
	if h.state["d"].Until.Sub(now) != 2*cooldownBase {
		t.Errorf("second failure should double: %s", h.state["d"].Until.Sub(now))
	}
	for i := 0; i < 10; i++ {
		h.fail("d", 0, now)
	}
	if h.state["d"].Until.Sub(now) != cooldownMax {
		t.Errorf("backoff must cap at %s, got %s", cooldownMax, h.state["d"].Until.Sub(now))
	}
	if !h.cooling("d", now) || h.cooling("d", now.Add(cooldownMax)) {
		t.Error("cooling window wrong")
	}
	h.ok("d")
	if h.cooling("d", now) {
		t.Error("ok must clear the cooldown")
	}
}

// Every deployment cooling: still try them (half-open), in priority order.
func TestAllCoolingStillTries(t *testing.T) {
	h := upstreamHealth
	a := &snapshot.Deployment{ID: "a"}
	b := &snapshot.Deployment{ID: "b"}
	now := time.Now()
	h.fail("a", 0, now)
	h.fail("b", 0, now)
	defer h.ok("a")
	defer h.ok("b")
	got := orderByHealth([]*snapshot.Deployment{a, b}, now)
	if len(got) != 2 || got[0] != a {
		t.Fatalf("order = %v", got)
	}
	h.ok("a")
	got = orderByHealth([]*snapshot.Deployment{b, a}, now)
	if got[0] != a || got[1] != b {
		t.Fatalf("healthy must come first: %v", got)
	}
}

// A pass-through 429 is one caller's plan limit: it still fails over, but must
// not cool the row every other subscriber shares. A 5xx still cools it.
func TestPassthrough429DoesNotCool(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)
	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(int(status.Load()))
	}))
	defer sub.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer backup.Close()

	d := stubDeployment(sub.URL, "claude-*", "sub-429", 0)
	d.Provider.AuthMode = "oauth_passthrough"
	b := stubDeployment(backup.URL, "claude-*", "backup-429", 1)
	defer upstreamHealth.ok(d.ID)
	defer upstreamHealth.ok(b.ID)
	ctx := provider.WithInbound(context.Background(), provider.Inbound{Authorization: "Bearer x"})
	body := func(*snapshot.Deployment) ([]byte, error) { return []byte(`{}`), nil }

	res, chosen, _, err := tryDeployments(ctx, []*snapshot.Deployment{d, b}, "/chat/completions", body)
	if err != nil || chosen != b {
		t.Fatalf("want failover to the backup, got %v, err %v", chosen, err)
	}
	res.Body.Close()
	if upstreamHealth.cooling(d.ID, time.Now()) {
		t.Error("a pass-through 429 cooled the shared row")
	}

	status.Store(http.StatusServiceUnavailable)
	res, _, _, _ = tryDeployments(ctx, []*snapshot.Deployment{d, b}, "/chat/completions", body)
	res.Body.Close()
	if !upstreamHealth.cooling(d.ID, time.Now()) {
		t.Error("a pass-through 5xx should still cool the row")
	}
}
