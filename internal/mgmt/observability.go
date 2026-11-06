package mgmt

import (
	"net/http"
)

// observabilityExported returns the in-memory ring of recently exported events,
// with ids resolved to names via the current snapshot.
func (m *Server) observabilityExported(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	if m.Recorder == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	provName, keyName, s := m.nameMaps()
	upstream := map[string]string{}
	if s != nil {
		for _, ds := range s.DeploymentsByModel {
			for _, d := range ds {
				upstream[d.ID] = d.UpstreamName
			}
		}
	}
	for _, e := range m.Recorder.Snapshot() {
		capture := "meta"
		if e.Capture {
			capture = "content"
		}
		out = append(out, map[string]any{
			"ts": e.TS, "key_name": keyName[e.KeyID], "model": e.Model,
			"upstream_name": upstream[e.DeploymentID], "provider_name": provName[e.ProviderID],
			"latency_ms": e.LatencyMS, "cost_usd": e.CostUSD, "rule_id": e.RuleID, "capture": capture,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// observabilityRulesStats returns per-rule live match rates (r/m) plus an
// optional "would this rule match now" preview for ?scope_type=&scope_value=.
func (m *Server) observabilityRulesStats(w http.ResponseWriter, r *http.Request) {
	rules := []map[string]any{}
	if m.Snapshots != nil && m.Live != nil {
		if s := m.Snapshots.Get(); s != nil {
			for _, rule := range s.Telemetry.Rules {
				rules = append(rules, map[string]any{
					"rule_id": rule.ID, "scope_type": rule.ScopeType, "scope_value": rule.ScopeValue,
					"enabled": rule.Enabled, "matching_rpm": m.Live.ScopeRPM(rule.ScopeType, rule.ScopeValue),
				})
			}
		}
	}
	// total_rpm sizes the blast radius of "everything" mode in the console.
	resp := map[string]any{"rules": rules}
	if m.Live != nil {
		resp["total_rpm"] = m.Live.TotalRPM()
	}
	st, sv := r.URL.Query().Get("scope_type"), r.URL.Query().Get("scope_value")
	if st != "" && sv != "" && m.Live != nil {
		resp["preview"] = map[string]any{"scope_type": st, "scope_value": sv, "matching_rpm": m.Live.ScopeRPM(st, sv)}
	}
	writeJSON(w, http.StatusOK, resp)
}
