package mgmt

import (
	"encoding/json"
	"time"

	"github.com/exitcodenihil/llm-router/internal/metrics"
)

// ReplicaState is what a control plane publishes to the others each second,
// so each one's console shows all of them: its own vitals, the gateways
// syncing with it, its live traffic view and its events.
type ReplicaState struct {
	Self     metrics.NodeMetric            `json:"self"`
	Gateways map[string]metrics.NodeMetric `json:"gateways,omitempty"`
	Live     metrics.Summary               `json:"live"`
	Events   []metrics.Event               `json:"events,omitempty"`
}

func (m *Server) selfMetric() metrics.NodeMetric {
	ver := m.Version
	if ver == "" {
		ver = "dev"
	}
	self := metrics.NodeMetric{Version: ver, CPUPct: metrics.CPUPct(), Updated: time.Now().UTC()}
	if !m.Started.IsZero() {
		self.UptimeS = int64(time.Since(m.Started).Seconds())
	}
	self.MemBytes, self.MemLimitBytes = metrics.Memory()
	if m.HTTP != nil {
		self.OpenConns, self.InFlight = m.HTTP.OpenConnections(), m.HTTP.InFlight()
	}
	return self
}

// ClusterState is this control plane's ReplicaState. Building it also takes
// in what the others published about the gateways syncing with them, so it is
// meant to be called once a second (cluster.Node.State).
func (m *Server) ClusterState() any {
	for _, p := range m.peerStates() {
		for id, nm := range p.Gateways {
			if m.NodeStore != nil {
				m.NodeStore.SetRemote(id, nm)
			}
		}
	}
	st := ReplicaState{Self: m.selfMetric()}
	if m.NodeStore != nil {
		st.Gateways = m.NodeStore.Recent()
	}
	if m.Live != nil {
		st.Live = m.Live.Summary()
	}
	if m.Events != nil {
		st.Events = m.Events.Snapshot()
	}
	return st
}

type peerState struct {
	ID string
	ReplicaState
}

// peerStates is what the other control planes last published.
func (m *Server) peerStates() []peerState {
	if m.Peers == nil {
		return nil
	}
	var out []peerState
	for _, p := range m.Peers() {
		ps := peerState{ID: p.ID}
		if json.Unmarshal(p.State, &ps.ReplicaState) == nil {
			out = append(out, ps)
		}
	}
	return out
}

// liveSummary is the live traffic view of every control plane together.
func (m *Server) liveSummary() metrics.Summary {
	all := []metrics.Summary{}
	if m.Live != nil {
		all = append(all, m.Live.Summary())
	}
	for _, p := range m.peerStates() {
		all = append(all, p.Live)
	}
	return metrics.Merge(all...)
}

// clusterEvents is every control plane's events, newest first.
func (m *Server) clusterEvents() []metrics.Event {
	var rings [][]metrics.Event
	if m.Events != nil {
		rings = append(rings, m.Events.Snapshot())
	}
	for _, p := range m.peerStates() {
		rings = append(rings, p.Events)
	}
	return metrics.MergedEvents(rings...)
}
