package metrics

import (
	"sync"
	"time"
)

// NodeMetric is the latest self-reported ops snapshot for one node.
// ponytail: in-memory only, keyed by node id, repopulated by heartbeats after a
// control-plane restart (no per-heartbeat DB write). cpu is best-effort.
type NodeMetric struct {
	Version string
	UptimeS int64
	CPUPct  float64
	MemPct  float64
	Updated time.Time
}

type NodeStore struct {
	mu sync.Mutex
	m  map[string]NodeMetric
}

func NewNodeStore() *NodeStore { return &NodeStore{m: map[string]NodeMetric{}} }

func (s *NodeStore) Set(id string, nm NodeMetric) {
	nm.Updated = time.Now()
	s.mu.Lock()
	s.m[id] = nm
	s.mu.Unlock()
}

func (s *NodeStore) Get(id string) (NodeMetric, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nm, ok := s.m[id]
	return nm, ok
}
