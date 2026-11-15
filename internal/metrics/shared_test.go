package metrics

import (
	"testing"
	"time"
)

func TestMergeAddsUp(t *testing.T) {
	a := Summary{
		Providers: []ProviderStat{{ID: "p", P50: 100, P95: 200, ErrPct: 10, RPM: 30, Count: 30}},
		Edges:     []EdgeStat{{KeyID: "k", Model: "m", ProviderID: "p", Volume: 30}},
		Nodes:     []NodeRPS{{ID: "", RPS: 0.5}},
		NodeP50:   map[string]int{"": 100},
		Scopes:    map[string]int{"team:t": 30},
		P50:       100,
	}
	b := Summary{
		Providers: []ProviderStat{{ID: "p", P50: 300, P95: 400, ErrPct: 0, RPM: 10, Count: 10}, {ID: "q", RPM: 5, Count: 5, P50: 50}},
		Edges:     []EdgeStat{{KeyID: "k", Model: "m", ProviderID: "p", Volume: 10}},
		Nodes:     []NodeRPS{{ID: "", RPS: 1.5}, {ID: "g1", RPS: 2}},
		NodeP50:   map[string]int{"": 300, "g1": 40},
		Scopes:    map[string]int{"team:t": 10, "key:k": 4},
		P50:       300,
	}
	m := Merge(a, b)
	if m.TotalRPM() != 45 || len(m.Providers) != 2 {
		t.Fatalf("total %d, providers %d", m.TotalRPM(), len(m.Providers))
	}
	p := m.Providers[0]
	if p.ID != "p" || p.Count != 40 || p.RPM != 40 || p.P50 != 150 || p.ErrPct != 7.5 {
		t.Fatalf("provider p: %+v", p)
	}
	if len(m.Edges) != 1 || m.Edges[0].Volume != 40 {
		t.Fatalf("edges %+v", m.Edges)
	}
	if m.NodeRPS("") != 2 || m.NodeRPS("g1") != 2 || m.NodeP50[""] != 250 || m.NodeP50["g1"] != 40 {
		t.Fatalf("nodes %+v %v", m.Nodes, m.NodeP50)
	}
	if m.Scopes["team:t"] != 40 || m.Scopes["key:k"] != 4 {
		t.Fatalf("scopes %v", m.Scopes)
	}
	// One view merges to itself.
	if one := Merge(a); one.TotalRPM() != 30 || one.P50 != 100 || one.Providers[0].P95 != 200 {
		t.Fatalf("single: %+v", one)
	}
}

// A gateway that syncs with another control plane shows here with its
// history, is not reported to Prometheus from here, and becomes this
// replica's again when it syncs here.
func TestRemoteGateways(t *testing.T) {
	s := NewNodeStore()
	now := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return now }
	s.Set("local", NodeMetric{Name: "a", Group: "g", InFlight: 1})
	s.SetRemote("remote", NodeMetric{Name: "b", Group: "g", InFlight: 2, Updated: now})
	if got := s.Recent(); len(got) != 1 || got["local"].Name != "a" {
		t.Fatalf("recent %v", got)
	}
	if cur := s.current(); len(cur) != 1 || cur[0].Name != "a" {
		t.Fatalf("reported to Prometheus: %+v", cur)
	}
	if nm, ok := s.Get("remote"); !ok || nm.InFlight != 2 {
		t.Fatalf("remote gateway unknown: %+v", nm)
	}
	if len(s.History()) != 2 {
		t.Fatalf("history for %d gateways", len(s.History()))
	}
	// An older report from a peer does not replace a sync here.
	s.SetRemote("local", NodeMetric{Name: "a", Group: "g", InFlight: 9, Updated: now.Add(-time.Second)})
	if nm, _ := s.Get("local"); nm.InFlight != 1 {
		t.Fatal("a stale peer report replaced a local sync")
	}
	// The remote gateway syncs here: it is ours.
	now = now.Add(time.Second)
	s.Set("remote", NodeMetric{Name: "b", Group: "g"})
	if len(s.Recent()) != 2 || len(s.current()) != 2 {
		t.Fatal("a gateway that came back is not reported")
	}
	now = now.Add(recentSync + time.Second)
	if len(s.Recent()) != 0 {
		t.Fatal("gateways not heard from are still published")
	}
}
