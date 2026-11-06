package gwsync

import (
	"net/http"

	"github.com/exitcodenihil/llm-router/internal/metrics"
)

// observe keeps a gateway's stats from its sync for the Gateways page and
// /metrics. The instance's name is read once per control-plane process.
func (s *Server) observe(r *http.Request, instance, group string, st Stats) {
	if s.Nodes == nil {
		return
	}
	prev, ok := s.Nodes.Get(instance)
	name := prev.Name
	if !ok || name == "" {
		s.Store.Pool.QueryRow(r.Context(), `SELECT name FROM gateway_instances WHERE id=$1`, instance).Scan(&name)
		if name == "" {
			name = instance
		}
	}
	s.Nodes.Set(instance, metrics.NodeMetric{Name: name, Group: group, Version: st.Version, UptimeS: st.UptimeS,
		CPUPct: st.CPUPct, InFlight: st.InFlight, Workers: st.Workers, OpenConns: st.OpenConnections,
		MemBytes: st.MemBytes, MemLimitBytes: st.MemLimitBytes, Leaving: st.Leaving})
}
