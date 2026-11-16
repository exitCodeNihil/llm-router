package metrics

import (
	"sort"
	"time"
)

// Summary is one control plane's live view, in a form several can be added
// up from: what it publishes to the others each second.
type Summary struct {
	Providers []ProviderStat `json:"providers,omitempty"`
	Edges     []EdgeStat     `json:"edges,omitempty"`
	Nodes     []NodeRPS      `json:"nodes,omitempty"`
	NodeP50   map[string]int `json:"node_p50,omitempty"` // by node id, "" for the control plane
	Scopes    map[string]int `json:"scopes,omitempty"`   // requests in the window by "type:value"
	P50       int            `json:"p50"`
}

// maxSharedEdges bounds what is published each second; the busiest stay.
const maxSharedEdges = 500

// Summary is everything the live pages read, at once.
func (l *Live) Summary() Summary {
	s := Summary{Providers: l.Providers(), Edges: l.Edges(), Nodes: l.Nodes(), P50: l.OverallP50(),
		NodeP50: map[string]int{}, Scopes: map[string]int{}}
	if len(s.Edges) > maxSharedEdges {
		sort.Slice(s.Edges, func(i, j int) bool { return s.Edges[i].Volume > s.Edges[j].Volume })
		s.Edges = s.Edges[:maxSharedEdges]
	}
	for _, n := range s.Nodes {
		s.NodeP50[n.ID] = l.NodeP50(n.ID)
	}
	sec := l.now().Unix()
	l.mu.Lock()
	for k, c := range l.scopes {
		if n, _ := c.totals(sec); n > 0 {
			s.Scopes[k] = n
		}
	}
	l.mu.Unlock()
	return s
}

// Merge adds up the views of several control planes. Counts and rates add;
// latencies are averaged by how many requests each saw, which is close when
// the replicas see similar traffic and is the best several medians allow.
func Merge(all ...Summary) Summary {
	out := Summary{NodeP50: map[string]int{}, Scopes: map[string]int{}}
	type lat struct{ p50, p95, n float64 }
	provs := map[string]*ProviderStat{}
	provLat := map[string]*lat{}
	edges := map[[3]string]int{}
	nodes := map[string]float64{}
	nodeLat := map[string]*lat{}
	var overall lat
	for _, s := range all {
		total := 0
		for _, p := range s.Providers {
			total += p.Count
			m := provs[p.ID]
			if m == nil {
				m = &ProviderStat{ID: p.ID}
				provs[p.ID], provLat[p.ID] = m, &lat{}
			}
			errs := m.ErrPct*float64(m.Count) + p.ErrPct*float64(p.Count)
			m.Count += p.Count
			m.RPM += p.RPM
			if m.Count > 0 {
				m.ErrPct = errs / float64(m.Count)
			}
			w := provLat[p.ID]
			w.p50 += float64(p.P50 * p.Count)
			w.p95 += float64(p.P95 * p.Count)
			w.n += float64(p.Count)
		}
		for _, e := range s.Edges {
			edges[[3]string{e.KeyID, e.Model, e.ProviderID}] += e.Volume
		}
		for _, n := range s.Nodes {
			nodes[n.ID] += n.RPS
			if w := nodeLat[n.ID]; w == nil {
				nodeLat[n.ID] = &lat{}
			}
			nodeLat[n.ID].p50 += float64(s.NodeP50[n.ID]) * n.RPS
			nodeLat[n.ID].n += n.RPS
		}
		for k, n := range s.Scopes {
			out.Scopes[k] += n
		}
		overall.p50 += float64(s.P50 * total)
		overall.n += float64(total)
	}
	for id, p := range provs {
		if w := provLat[id]; w.n > 0 {
			p.P50, p.P95 = int(w.p50/w.n), int(w.p95/w.n)
		}
		out.Providers = append(out.Providers, *p)
	}
	sort.Slice(out.Providers, func(i, j int) bool { return out.Providers[i].ID < out.Providers[j].ID })
	for k, v := range edges {
		out.Edges = append(out.Edges, EdgeStat{KeyID: k[0], Model: k[1], ProviderID: k[2], Volume: v})
	}
	for id, rps := range nodes {
		out.Nodes = append(out.Nodes, NodeRPS{ID: id, RPS: rps})
		if w := nodeLat[id]; w.n > 0 {
			out.NodeP50[id] = int(w.p50 / w.n)
		}
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	if overall.n > 0 {
		out.P50 = int(overall.p50 / overall.n)
	}
	return out
}

// TotalRPM is the requests over the window, all providers.
func (s Summary) TotalRPM() int {
	n := 0
	for _, p := range s.Providers {
		n += p.Count
	}
	return n
}

// NodeRPS is one node's request rate ("" for the control planes' own).
func (s Summary) NodeRPS(id string) float64 {
	for _, n := range s.Nodes {
		if n.ID == id {
			return n.RPS
		}
	}
	return 0
}

// --- gateways that sync with another control plane ---

// recentSync is how lately a gateway must have synced here to be this
// replica's to report: three sync intervals.
const recentSync = 6 * time.Second

// Recent returns the gateways that synced with this process in the last few
// seconds, by instance id: the ones it publishes to the other replicas.
func (s *NodeStore) Recent() map[string]NodeMetric {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]NodeMetric{}
	for id, n := range s.m {
		if !n.remote && now.Sub(n.Updated) <= recentSync {
			out[id] = n.NodeMetric
		}
	}
	return out
}

// SetRemote records a gateway's sync with another control plane, unless this
// one heard from it since. It keeps the page and the last-hour graphs whole on
// every replica; /metrics reports a gateway only from the replica it syncs
// with, so a scrape of all of them counts each once.
func (s *NodeStore) SetRemote(id string, nm NodeMetric) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.m[id]
	if n == nil {
		n = &node{}
		s.m[id] = n
	} else if !nm.Updated.After(n.Updated) {
		return
	}
	n.NodeMetric, n.remote = nm, true
	n.record(nm)
}

// MergedEvents puts several control planes' event rings together, newest
// first.
func MergedEvents(rings ...[]Event) []Event {
	var out []Event
	for _, r := range rings {
		out = append(out, r...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	if len(out) > eventsRingSize {
		out = out[:eventsRingSize]
	}
	return out
}
