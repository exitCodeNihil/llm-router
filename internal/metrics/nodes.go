package metrics

import (
	"sort"
	"sync"
	"time"
)

// NodeMetric is the latest self-reported ops snapshot of one gateway.
// ponytail: in-memory only, keyed by instance id, repopulated by syncs after a
// control-plane restart (no per-sync DB write). Each control-plane replica
// knows the gateways that sync with it.
type NodeMetric struct {
	Name, Group   string // the instance's and its group's names
	Version       string
	UptimeS       int64
	CPUPct        float64
	InFlight      int64
	Workers       int
	OpenConns     int64
	MemBytes      int64
	MemLimitBytes int64 // 0: no limit
	Leaving       bool  // shutting down: these are its last syncs
	Updated       time.Time
}

// MemPct is memory in use against the limit; 0 without a limit.
func (n NodeMetric) MemPct() float64 {
	if n.MemLimitBytes <= 0 {
		return 0
	}
	return float64(n.MemBytes) / float64(n.MemLimitBytes) * 100
}

const (
	// HistoryStep and HistoryLen: one sample per 10 s for an hour.
	HistoryStep = 10 * time.Second
	HistoryLen  = 360
	// A gateway that stopped syncing without saying it was leaving is
	// reported for staleAfter (so it can be alerted on), one that left for
	// leftAfter. A gateway gone longer than forgetAfter is forgotten, and so
	// is its history.
	staleAfter  = 5 * time.Minute
	leftAfter   = 15 * time.Second
	forgetAfter = 10 * time.Minute // as long as the Gateways page lists it
)

// Sample is the peak of each value over one HistoryStep.
type Sample struct {
	TS            int64 `json:"ts"` // unix seconds, the start of the step
	OpenConns     int64 `json:"open_connections"`
	InFlight      int64 `json:"in_flight"`
	MemBytes      int64 `json:"mem_bytes"`
	MemLimitBytes int64 `json:"mem_limit_bytes"`
}

type node struct {
	NodeMetric
	hist []Sample // ring of HistoryLen at most
	head int      // the oldest sample's index once the ring is full
}

func (n *node) record(nm NodeMetric) {
	s := Sample{TS: nm.Updated.Truncate(HistoryStep).Unix(), OpenConns: nm.OpenConns, InFlight: nm.InFlight,
		MemBytes: nm.MemBytes, MemLimitBytes: nm.MemLimitBytes}
	if len(n.hist) > 0 {
		last := &n.hist[(n.head+len(n.hist)-1)%len(n.hist)]
		if last.TS == s.TS {
			last.OpenConns = max(last.OpenConns, s.OpenConns)
			last.InFlight = max(last.InFlight, s.InFlight)
			last.MemBytes = max(last.MemBytes, s.MemBytes)
			last.MemLimitBytes = s.MemLimitBytes
			return
		}
	}
	if len(n.hist) < HistoryLen {
		n.hist = append(n.hist, s)
		return
	}
	n.hist[n.head] = s
	n.head = (n.head + 1) % HistoryLen
}

func (n *node) history(since int64) []Sample {
	out := make([]Sample, 0, len(n.hist))
	for i := range n.hist {
		if s := n.hist[(n.head+i)%len(n.hist)]; s.TS >= since {
			out = append(out, s)
		}
	}
	return out
}

// NodeStore keeps every gateway's latest sync and its last hour.
type NodeStore struct {
	mu     sync.Mutex
	now    func() time.Time
	m      map[string]*node
	pruned time.Time
}

func NewNodeStore() *NodeStore { return &NodeStore{now: time.Now, m: map[string]*node{}} }

// Set records a gateway's sync.
func (s *NodeStore) Set(id string, nm NodeMetric) {
	now := s.now()
	nm.Updated = now
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.m[id]
	if n == nil {
		n = &node{}
		s.m[id] = n
	}
	n.NodeMetric = nm
	n.record(nm)
	if now.Sub(s.pruned) > time.Minute {
		s.pruned = now
		for id, n := range s.m {
			if now.Sub(n.Updated) > forgetAfter {
				delete(s.m, id)
			}
		}
	}
}

func (s *NodeStore) Get(id string) (NodeMetric, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.m[id]
	if !ok {
		return NodeMetric{}, false
	}
	return n.NodeMetric, true
}

// NodeHistory is one gateway's samples over the last hour, oldest first.
type NodeHistory struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Group   string   `json:"group"`
	Samples []Sample `json:"samples"`
}

func byGroupAndName[T any](s []T, key func(T) (string, string)) {
	sort.Slice(s, func(i, j int) bool {
		gi, ni := key(s[i])
		gj, nj := key(s[j])
		if gi != gj {
			return gi < gj
		}
		return ni < nj
	})
}

// History returns every known gateway's last hour, by group and name.
func (s *NodeStore) History() []NodeHistory {
	now := s.now()
	since := now.Add(-HistoryLen * HistoryStep).Unix()
	s.mu.Lock()
	out := make([]NodeHistory, 0, len(s.m))
	for id, n := range s.m {
		if now.Sub(n.Updated) <= forgetAfter {
			out = append(out, NodeHistory{ID: id, Name: n.Name, Group: n.Group, Samples: n.history(since)})
		}
	}
	s.mu.Unlock()
	byGroupAndName(out, func(h NodeHistory) (string, string) { return h.Group, h.Name })
	return out
}

// current returns the gateways to report to Prometheus: not gone, and one
// per group and name (a restarted gateway joins again under a new id with
// the same name; the newest wins).
func (s *NodeStore) current() []NodeMetric {
	now := s.now()
	type gk struct{ group, name string }
	latest := map[gk]NodeMetric{}
	s.mu.Lock()
	for _, n := range s.m {
		age := now.Sub(n.Updated)
		if age > staleAfter || (n.Leaving && age > leftAfter) {
			continue
		}
		k := gk{n.Group, n.Name}
		if prev, ok := latest[k]; !ok || n.Updated.After(prev.Updated) {
			latest[k] = n.NodeMetric
		}
	}
	s.mu.Unlock()
	out := make([]NodeMetric, 0, len(latest))
	for _, nm := range latest {
		out = append(out, nm)
	}
	byGroupAndName(out, func(n NodeMetric) (string, string) { return n.Group, n.Name })
	return out
}

// Collect writes one series per gateway from its latest sync, labelled with
// its group's and its own name, so one scrape of the control plane gives the
// fleet. A gateway's series go once it is gone.
func (s *NodeStore) Collect(w *PromWriter) {
	now := s.now()
	nodes := s.current()
	perGroup := map[string]int{}
	var groups []string
	for _, n := range nodes {
		if perGroup[n.Group] == 0 {
			groups = append(groups, n.Group)
		}
		perGroup[n.Group]++
	}
	w.Family("llmr_gateways_connected", "gauge", "Gateways syncing with this control plane")
	for _, g := range groups {
		w.Sample("llmr_gateways_connected", float64(perGroup[g]), "group", g)
	}
	family := func(name, help string, v func(NodeMetric) (float64, bool), extra ...func(NodeMetric) []string) {
		w.Family(name, "gauge", help)
		for _, n := range nodes {
			if x, ok := v(n); ok {
				labels := []string{"group", n.Group, "gateway", n.Name}
				for _, e := range extra {
					labels = append(labels, e(n)...)
				}
				w.Sample(name, x, labels...)
			}
		}
	}
	always := func(f func(NodeMetric) float64) func(NodeMetric) (float64, bool) {
		return func(n NodeMetric) (float64, bool) { return f(n), true }
	}
	family("llmr_gateway_open_connections", "Client connections open on a gateway, at its last sync",
		always(func(n NodeMetric) float64 { return float64(n.OpenConns) }))
	family("llmr_gateway_in_flight", "Requests a gateway was serving, at its last sync",
		always(func(n NodeMetric) float64 { return float64(n.InFlight) }))
	family("llmr_gateway_memory_bytes", "A gateway's memory in use, at its last sync",
		always(func(n NodeMetric) float64 { return float64(n.MemBytes) }))
	family("llmr_gateway_memory_limit_bytes", "A gateway's memory limit (absent: no limit)",
		func(n NodeMetric) (float64, bool) { return float64(n.MemLimitBytes), n.MemLimitBytes > 0 })
	family("llmr_gateway_last_sync_age_seconds", "Seconds since a gateway last synced",
		always(func(n NodeMetric) float64 { return now.Sub(n.Updated).Seconds() }))
	family("llmr_gateway_info", "A gateway's version",
		always(func(NodeMetric) float64 { return 1 }), func(n NodeMetric) []string { return []string{"version", n.Version} })
}
