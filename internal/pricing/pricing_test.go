package pricing

import (
	"encoding/json"
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
	cost, unpriced := Cost(s, d, 1000, 100, 200)
	want := 800*2.5/1e6 + 100*10.0/1e6 + 200*1.25/1e6
	if unpriced || cost < want-1e-12 || cost > want+1e-12 {
		t.Errorf("catalog cost = %v (unpriced=%v), want %v", cost, unpriced, want)
	}

	// custom deployment pricing wins over catalog
	in, out := 0.1, 0.2
	d2 := &snapshot.Deployment{CatalogModelID: "azure/gpt-4o", InputPer1M: &in, OutputPer1M: &out}
	cost, unpriced = Cost(s, d2, 1_000_000, 1_000_000, 0)
	if unpriced || cost < 0.3-1e-12 || cost > 0.3+1e-12 {
		t.Errorf("custom cost = %v (unpriced=%v), want 0.3", cost, unpriced)
	}

	// custom pricing honours its own cached rate: 800 fresh + 200 cached
	dc := 0.01
	d3 := &snapshot.Deployment{InputPer1M: &in, OutputPer1M: &out, CachedInputPer1M: &dc}
	cost, unpriced = Cost(s, d3, 1000, 100, 200)
	want = 800*0.1/1e6 + 100*0.2/1e6 + 200*0.01/1e6
	if unpriced || cost < want-1e-12 || cost > want+1e-12 {
		t.Errorf("custom cached cost = %v (unpriced=%v), want %v", cost, unpriced, want)
	}

	// ...and without one, cache reads still bill at the full input rate
	cost, _ = Cost(s, d2, 1000, 100, 200)
	want = 1000*0.1/1e6 + 100*0.2/1e6
	if cost < want-1e-12 || cost > want+1e-12 {
		t.Errorf("custom uncached cost = %v, want %v", cost, want)
	}

	// no pricing anywhere → unpriced
	if _, unpriced = Cost(s, &snapshot.Deployment{}, 10, 10, 0); !unpriced {
		t.Error("expected unpriced for deployment with no pricing")
	}
}
