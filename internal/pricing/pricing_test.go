package pricing

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

func TestSeedCatalogParses(t *testing.T) {
	var entries map[string]seedEntry
	if err := json.Unmarshal(seedJSON, &entries); err != nil {
		t.Fatalf("embedded catalog invalid: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("embedded catalog is empty")
	}
}

func TestCostResolution(t *testing.T) {
	cached := 1.25
	s := &snapshot.Snapshot{Prices: map[string]snapshot.Price{
		"azure/gpt-4o": {InputPer1M: 2.5, OutputPer1M: 10, CachedInputPer1M: &cached},
	}}

	// catalog pricing with cached tokens: 800 fresh + 200 cached prompt, 100 completion
	d := &snapshot.Deployment{CatalogModelID: "azure/gpt-4o"}
	cost, unpriced := Cost(s, d, Tokens{Prompt: 1000, Completion: 100, Cached: 200})
	want := 800*2.5/1e6 + 100*10.0/1e6 + 200*1.25/1e6
	if unpriced || cost < want-1e-12 || cost > want+1e-12 {
		t.Errorf("catalog cost = %v (unpriced=%v), want %v", cost, unpriced, want)
	}

	// custom deployment pricing wins over catalog
	in, out := 0.1, 0.2
	d2 := &snapshot.Deployment{CatalogModelID: "azure/gpt-4o", InputPer1M: &in, OutputPer1M: &out}
	cost, unpriced = Cost(s, d2, Tokens{Prompt: 1_000_000, Completion: 1_000_000})
	if unpriced || cost < 0.3-1e-12 || cost > 0.3+1e-12 {
		t.Errorf("custom cost = %v (unpriced=%v), want 0.3", cost, unpriced)
	}

	// custom pricing honours its own cached rate: 800 fresh + 200 cached
	dc := 0.01
	d3 := &snapshot.Deployment{InputPer1M: &in, OutputPer1M: &out, CachedInputPer1M: &dc}
	cost, unpriced = Cost(s, d3, Tokens{Prompt: 1000, Completion: 100, Cached: 200})
	want = 800*0.1/1e6 + 100*0.2/1e6 + 200*0.01/1e6
	if unpriced || cost < want-1e-12 || cost > want+1e-12 {
		t.Errorf("custom cached cost = %v (unpriced=%v), want %v", cost, unpriced, want)
	}

	// ...and without one, cache reads still bill at the full input rate
	cost, _ = Cost(s, d2, Tokens{Prompt: 1000, Completion: 100, Cached: 200})
	want = 1000*0.1/1e6 + 100*0.2/1e6
	if cost < want-1e-12 || cost > want+1e-12 {
		t.Errorf("custom uncached cost = %v, want %v", cost, want)
	}

	// no pricing anywhere → unpriced
	if _, unpriced = Cost(s, &snapshot.Deployment{}, Tokens{Prompt: 10, Completion: 10}); !unpriced {
		t.Error("expected unpriced for deployment with no pricing")
	}
}

// costCase is one shared fixture: gateway/src/usage.rs checks the Rust pricing
// against the same file, so the two can't drift apart.
type costCase struct {
	Name   string    `json:"name"`
	Rates  []float64 `json:"rates"` // input, output, cached ($/1M)
	Long   []float64 `json:"long"`  // threshold, multiplier
	Tokens struct {
		Prompt     int  `json:"prompt"`
		Completion int  `json:"completion"`
		Cached     int  `json:"cached"`
		Write5m    int  `json:"write_5m"`
		Write1h    int  `json:"write_1h"`
		US         bool `json:"us"`
		Fast       bool `json:"fast"`
	} `json:"tokens"`
	Want float64 `json:"want"`
}

func TestCostFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/cost.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []costCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		p := snapshot.Price{InputPer1M: c.Rates[0], OutputPer1M: c.Rates[1]}
		if len(c.Rates) > 2 {
			p.CachedInputPer1M = &c.Rates[2]
		}
		if len(c.Long) > 0 {
			p.LongPromptTokens, p.LongPromptMultiplier = int(c.Long[0]), c.Long[1]
		}
		s := &snapshot.Snapshot{Prices: map[string]snapshot.Price{"m": p}}
		got, unpriced := Cost(s, &snapshot.Deployment{CatalogModelID: "m"}, Tokens{
			Prompt: c.Tokens.Prompt, Completion: c.Tokens.Completion, Cached: c.Tokens.Cached,
			Write5m: c.Tokens.Write5m, Write1h: c.Tokens.Write1h, USInference: c.Tokens.US, Fast: c.Tokens.Fast,
		})
		if unpriced || math.Abs(got-c.Want) > 1e-12 {
			t.Errorf("%s: cost = %v (unpriced=%v), want %v", c.Name, got, unpriced, c.Want)
		}
	}
}

// TestAnthropicCatalog pins the shipped catalog to Anthropic's published prices
// (platform.claude.com/docs/en/about-claude/pricing): input, output, cache read.
// A failure here means one of the two changed: check the page before updating it.
func TestAnthropicCatalog(t *testing.T) {
	published := map[string][3]float64{
		"claude-fable-5-1":           {10, 50, 0.25},
		"claude-fable-5":             {10, 50, 1},
		"claude-opus-5-5":            {4, 20, 0.2},
		"claude-opus-5":              {5, 25, 0.5},
		"claude-opus-4-8":            {5, 25, 0.5},
		"claude-opus-4-7":            {5, 25, 0.5},
		"claude-opus-4-6":            {5, 25, 0.5},
		"claude-opus-4-5-20251101":   {5, 25, 0.5},
		"claude-sonnet-5-5":          {2, 10, 0.2},
		"claude-sonnet-5":            {2, 10, 0.2},
		"claude-sonnet-4-6":          {3, 15, 0.3},
		"claude-sonnet-4-5-20250929": {3, 15, 0.3},
		"claude-haiku-5-5":           {0.1, 0.5, 0.01},
		"claude-haiku-4-5-20251001":  {1, 5, 0.1},
	}
	var catalog map[string]seedEntry
	if err := json.Unmarshal(seedJSON, &catalog); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for id, e := range catalog {
		model, ok := strings.CutPrefix(id, "anthropic/")
		if !ok {
			continue
		}
		seen++
		want, ok := published[model]
		if !ok {
			t.Errorf("%s is in the catalog but not in this table", id)
			continue
		}
		if e.InputPer1M != want[0] || e.OutputPer1M != want[1] || e.CachedInputPer1M == nil || *e.CachedInputPer1M != want[2] {
			t.Errorf("%s = %+v, published %v", id, e, want)
		}
	}
	if seen != len(published) {
		t.Errorf("catalog has %d anthropic/ models, published table %d", seen, len(published))
	}
	if h := catalog["anthropic/claude-haiku-5-5"]; h.LongPromptTokens == nil || *h.LongPromptTokens != 100000 || *h.LongPromptMult != 5 {
		t.Errorf("Haiku 5.5's long prompt tier is missing: %+v", h)
	}
}

func TestNotionalUsesTheCatalogForWildcards(t *testing.T) {
	cached := 0.1
	s := &snapshot.Snapshot{Prices: map[string]snapshot.Price{
		"anthropic/claude-x": {InputPer1M: 1, OutputPer1M: 5, CachedInputPer1M: &cached},
	}}
	d := &snapshot.Deployment{Provider: &snapshot.Provider{AuthMode: "oauth_passthrough"}}
	tok := Tokens{Prompt: 1000, Completion: 100, Write5m: 1000}
	if got := Notional(s, d, "claude-x", tok); math.Abs(got-0.00175) > 1e-12 {
		t.Errorf("notional = %v, want 0.00175", got)
	}
	if got := Notional(s, d, "not-in-catalog", tok); got != 0 {
		t.Errorf("unknown model: notional = %v, want 0", got)
	}
	if cost, unpriced := Cost(s, d, tok); cost != 0 || !unpriced {
		t.Errorf("pass-through must stay unpriced: %v %v", cost, unpriced)
	}
}

// Claude in Microsoft Foundry bills at Anthropic's own rates, and Vertex's
// global endpoint lists the current models at the same rates, so every
// current model has a row on each platform matching its anthropic/ row.
func TestCurrentClaudeModelsArePricedOnEveryPlatform(t *testing.T) {
	var catalog map[string]seedEntry
	if err := json.Unmarshal(seedJSON, &catalog); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"claude-fable-5-1", "claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"} {
		want, ok := catalog["anthropic/"+model]
		if !ok {
			t.Fatalf("anthropic/%s missing", model)
		}
		for _, platform := range []string{"azure/", "gcp/"} {
			got, ok := catalog[platform+model]
			if !ok {
				t.Errorf("%s%s missing: deployments using it would be recorded as unpriced", platform, model)
				continue
			}
			wj, _ := json.Marshal(want)
			gj, _ := json.Marshal(got)
			if string(wj) != string(gj) {
				t.Errorf("%s%s = %s, anthropic/ %s", platform, model, gj, wj)
			}
		}
	}
}
