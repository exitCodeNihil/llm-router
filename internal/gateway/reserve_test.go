package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/limits"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

func TestTextTokensSkipsEncodedMedia(t *testing.T) {
	text := `{"messages":[{"role":"user","content":"` + strings.Repeat("word ", 400) + `"}]}`
	if got := textTokens([]byte(text)); got < 450 || got > 520 {
		t.Fatalf("text: %d tokens", got)
	}
	image := `{"messages":[{"role":"user","content":[{"type":"image","source":{"data":"` +
		strings.Repeat("A", 2<<20) + `"}},{"type":"text","text":"what is this?"}]}]}`
	if got := textTokens([]byte(image)); got > 50 {
		t.Fatalf("a 2 MiB image counted as %d tokens", got)
	}
	// A long text with an escaped quote is still text.
	long := `{"x":"` + strings.Repeat(`he said \"hi\" `, 1000) + `"}`
	if got := textTokens([]byte(long)); got < 3000 {
		t.Fatalf("long text: %d tokens", got)
	}
}

func TestExpectedOutput(t *testing.T) {
	var o outputStats
	if o.expected("m", 0) != defaultOutputTokens || o.expected("m", 64000) != defaultOutputTokens || o.expected("m", 100) != 100 {
		t.Fatal("before any sample: the default, or less if less was asked for")
	}
	for range outputSamples {
		o.add("m", 300)
	}
	if o.expected("m", 64000) != 600 || o.expected("m", 200) != 200 || o.expected("other", 0) != defaultOutputTokens {
		t.Fatalf("after samples: %d", o.expected("m", 64000))
	}
}

// Ten requests at once on a budget with room for about two: the rest are
// turned away with budget_reserved while the first are in flight, and are
// admitted again once those finish.
func TestParallelRequestsHoldTheBudget(t *testing.T) {
	gate := make(chan struct{})
	var arrived sync.WaitGroup
	arrived.Add(2)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Done()
		<-gate
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`))
	}))
	defer up.Close()
	g, cw, secret := newTestServer(t, map[string][]*snapshot.Deployment{"m": {stubDeployment(up.URL, "m", "m", 1)}})
	snap := g.Snapshots.Get()
	// $10 per 1M output: a request asking for 1000 tokens holds one cent.
	budget := 0.02
	for _, k := range snap.KeysByHash {
		k.BudgetUSD = &budget
	}
	snap.Spend = map[string]snapshot.Spend{}
	lim := limits.New()
	g.Limits = lim
	body := `{"model":"m","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`

	codes := make(chan int, 2)
	for range 2 {
		go func() { codes <- doReq(t, g, secret, body).Code }()
	}
	arrived.Wait() // two in flight: the whole two cents are held
	for range 8 {
		rec := doReq(t, g, secret, body)
		if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "budget_reserved") ||
			rec.Header().Get("Retry-After") != "1" {
			t.Fatalf("a third request in parallel: %d %s", rec.Code, rec.Body.String())
		}
	}
	close(gate)
	for range 2 {
		if c := <-codes; c != http.StatusOK {
			t.Fatalf("an admitted request ended %d", c)
		}
	}
	if lim.Held() != 0 {
		t.Fatalf("still holding $%v with nothing in flight", lim.Held())
	}
	arrived.Add(1)
	if rec := doReq(t, g, secret, body); rec.Code != http.StatusOK {
		t.Fatalf("refused after the others finished: %d %s", rec.Code, rec.Body.String())
	}
	refused := 0
	for _, e := range cw.events {
		if e.ErrorCode == "budget_reserved" {
			refused++
		}
	}
	if refused != 8 {
		t.Fatalf("%d refusals recorded, want 8", refused)
	}
}
