package snapshot

import "testing"

func f(v float64) *float64 { return &v }

func TestSortDeploymentsPriorityThenPrice(t *testing.T) {
	s := &Snapshot{
		DeploymentsByModel: map[string][]*Deployment{},
		Prices:             map[string]Price{"cat/cheap": {InputPer1M: 1, OutputPer1M: 2}},
	}
	s.DeploymentsByModel["m"] = []*Deployment{
		{UpstreamName: "unpriced", Priority: 0},
		{UpstreamName: "expensive", Priority: 0, InputPer1M: f(10), OutputPer1M: f(50)},
		{UpstreamName: "catalog-cheap", Priority: 0, CatalogModelID: "cat/cheap"},
		{UpstreamName: "fallback", Priority: 1, InputPer1M: f(0), OutputPer1M: f(0)},
		{UpstreamName: "missing-catalog", Priority: 0, CatalogModelID: "cat/none"},
	}
	s.SortDeployments()
	var got []string
	for _, d := range s.DeploymentsByModel["m"] {
		got = append(got, d.UpstreamName)
	}
	// Priority 1 stays behind every priority 0 even though it is free; within
	// priority 0 the cheapest priced backend leads and unpriced ones trail.
	want := []string{"catalog-cheap", "expensive", "missing-catalog", "unpriced", "fallback"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
