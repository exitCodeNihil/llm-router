package mgmt

import (
	"net/http"
	"time"

	"github.com/exitcodenihil/llm-router/internal/metrics"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// nameMaps resolves ids→names from the current snapshot for the live surfaces.
func (m *Server) nameMaps() (provName, keyName map[string]string, s *snapshot.Snapshot) {
	provName, keyName = map[string]string{}, map[string]string{}
	if m.Snapshots != nil {
		s = m.Snapshots.Get()
	}
	if s == nil {
		return
	}
	for _, ds := range s.DeploymentsByModel {
		for _, d := range ds {
			if d.Provider != nil {
				provName[d.Provider.ID] = d.Provider.Name
			}
		}
	}
	for _, k := range s.KeysByHash {
		keyName[k.ID] = k.Name
	}
	return
}

func ledFor(errPct float64) string {
	switch {
	case errPct > 10:
		return "err"
	case errPct > 2:
		return "warn"
	default:
		return "ok"
	}
}

func (m *Server) metricsLive(w http.ResponseWriter, r *http.Request) {
	if m.Live == nil {
		writeJSON(w, http.StatusOK, map[string]any{"providers": []any{}, "edges": []any{}, "nodes": []any{}, "totals": map[string]any{}})
		return
	}
	provName, keyName, s := m.nameMaps()
	total := m.Live.TotalRPM()

	providers := []map[string]any{}
	for _, p := range m.Live.Providers() {
		share := 0.0
		if total > 0 {
			share = float64(p.Count) / float64(total) * 100
		}
		providers = append(providers, map[string]any{
			"id": p.ID, "name": provName[p.ID], "p50_ms": p.P50, "p95_ms": p.P95,
			"err_pct": round1(p.ErrPct), "rpm": p.RPM, "share_pct": round1(share), "led": ledFor(p.ErrPct),
		})
	}
	edges := []map[string]any{}
	for _, e := range m.Live.Edges() {
		edges = append(edges, map[string]any{
			"key_id": e.KeyID, "key_name": keyName[e.KeyID], "model": e.Model,
			"provider_id": e.ProviderID, "provider_name": provName[e.ProviderID], "volume": e.Volume,
		})
	}
	nodeNames := m.gatewayNames(r)
	nodes := []map[string]any{}
	for _, n := range m.Live.Nodes() {
		name := nodeNames[n.ID]
		if n.ID == "" {
			name = "control-plane"
		}
		nodes = append(nodes, map[string]any{"id": n.ID, "name": name, "rps": round1(n.RPS)})
	}

	keys, aliases, upstreams := 0, 0, len(provName)
	if s != nil {
		keys, aliases = len(s.KeysByHash), len(s.DeploymentsByModel)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": providers, "edges": edges, "nodes": nodes,
		"totals": map[string]any{"rpm": total, "keys": keys, "aliases": aliases, "upstreams": upstreams},
	})
}

// gatewayNames maps gateway instance ids to "group / instance" (best-effort).
func (m *Server) gatewayNames(r *http.Request) map[string]string {
	out := map[string]string{}
	rows, err := listRows(r.Context(), m.Store.Pool,
		`SELECT i.id, g.name || ' / ' || i.name AS name FROM gateway_instances i JOIN gateway_groups g ON g.id = i.group_id`)
	if err != nil {
		return out
	}
	for _, row := range rows {
		if id, ok := row["id"].(string); ok {
			out[id], _ = row["name"].(string)
		}
	}
	return out
}

const p50SLOms = 2000 // upstream p50 above this raises a cluster event

// listNodes returns the control plane and the gateway instances with their ops
// metrics, plus the cluster-events ring.
func (m *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	selfVer := m.Version
	if selfVer == "" {
		selfVer = "dev"
	}
	selfUptime := int64(0)
	if !m.Started.IsZero() {
		selfUptime = int64(time.Since(m.Started).Seconds())
	}
	p50, selfP50 := 0, 0
	if m.Live != nil {
		p50, selfP50 = m.Live.OverallP50(), m.Live.NodeP50("")
	}
	self := metrics.NodeMetric{Version: selfVer, UptimeS: selfUptime, CPUPct: metrics.CPUPct()}
	self.MemBytes, self.MemLimitBytes = metrics.Memory()
	if m.HTTP != nil {
		self.OpenConns, self.InFlight = m.HTTP.OpenConnections(), m.HTTP.InFlight()
	}
	selfRow := nodeRow(self)
	for k, v := range map[string]any{"id": "self", "name": "control-plane", "addr": "", "role": "leader",
		"version_skew": false, "rps": round1(m.liveRPS("")), "p50_ms": selfP50, "last_seen": time.Now().UTC(),
		"status": "ok"} {
		selfRow[k] = v
	}
	nodes := []map[string]any{selfRow}

	now := time.Now()
	// Gateway instances sync every 2 s, so a few missed syncs already mean trouble.
	rows, _ := listRows(r.Context(), m.Store.Pool,
		`SELECT i.id, i.name, i.group_id, g.name AS group_name, g.region, i.last_seen_at
		FROM gateway_instances i JOIN gateway_groups g ON g.id = i.group_id
		WHERE i.last_seen_at > now() - interval '10 minutes' ORDER BY g.name, i.joined_at`)
	for _, row := range rows {
		id, _ := row["id"].(string)
		name, _ := row["name"].(string)
		group, _ := row["group_name"].(string)
		lastSeen, _ := row["last_seen_at"].(time.Time)
		var nm metrics.NodeMetric
		if m.NodeStore != nil {
			nm, _ = m.NodeStore.Get(id)
		}
		skew := nm.Version != "" && nm.Version != selfVer
		status := "ok"
		switch {
		case now.Sub(lastSeen) > 30*time.Second:
			status = "err"
		case now.Sub(lastSeen) > 10*time.Second || skew:
			status = "warn"
		}
		gw := nodeRow(nm)
		groupID, _ := row["group_id"].(string)
		gw["unserved_providers"] = []string{}
		if m.GatewaySync != nil {
			gw["unserved_providers"] = m.GatewaySync.UnservedProviders(groupID, nm.Schema)
		}
		p50 := 0
		if m.Live != nil {
			p50 = m.Live.NodeP50(id)
		}
		for k, v := range map[string]any{"id": id, "name": name, "addr": "", "role": "gateway", "group": group,
			"region": row["region"], "version_skew": skew, "rps": round1(m.liveRPS(id)), "p50_ms": p50,
			"last_seen": row["last_seen_at"], "status": status} {
			gw[k] = v
		}
		nodes = append(nodes, gw)
	}

	events := []map[string]any{}
	if m.Events != nil {
		if p50 > p50SLOms {
			m.Events.AddDedup("warn", "upstream p50 above SLO")
		}
		for _, e := range m.Events.Snapshot() {
			events = append(events, map[string]any{"ts": e.TS, "text": e.Text, "level": e.Level})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "cluster_events": events})
}

// nodeRow is the ops part of a node's row on the Gateways page.
func nodeRow(nm metrics.NodeMetric) map[string]any {
	return map[string]any{
		"version": nm.Version, "uptime_s": nm.UptimeS, "cpu_pct": round1(nm.CPUPct), "mem_pct": round1(nm.MemPct()),
		"mem_bytes": nm.MemBytes, "mem_limit_bytes": nm.MemLimitBytes, "open_connections": nm.OpenConns,
		"in_flight": nm.InFlight,
	}
}

// nodesHistory returns each gateway's last hour of open connections, requests
// in flight and memory, one sample per step (the peak over the step). Steps
// without a sync are missing. Per process, like the other live metrics.
func (m *Server) nodesHistory(w http.ResponseWriter, r *http.Request) {
	gateways := []metrics.NodeHistory{}
	if m.NodeStore != nil {
		gateways = m.NodeStore.History()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"step_s":   int(metrics.HistoryStep.Seconds()),
		"window_s": int((metrics.HistoryLen * metrics.HistoryStep).Seconds()),
		"gateways": gateways,
	})
}

func (m *Server) liveRPS(id string) float64 {
	if m.Live == nil {
		return 0
	}
	return m.Live.NodeRPS(id)
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
