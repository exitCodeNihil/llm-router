package mgmt

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// observabilityTest validates the telemetry destination: first that the host is
// reachable, then that the credentials are actually accepted. Checking only
// reachability is worse than useless here — Langfuse's /api/public/health needs
// no auth and answers 200 for any credentials, so a wrong key still looked
// "reachable" while every export was silently rejected with a 401.
func (m *Server) observabilityTest(w http.ResponseWriter, r *http.Request) {
	cfg, ok := decode[snapshot.Telemetry](w, r)
	if !ok {
		return
	}
	if cfg.SecretKey == "" || cfg.PublicKey == "" || cfg.Host == "" {
		// fall back to the stored config for anything omitted
		var raw []byte
		if err := m.Store.Pool.QueryRow(r.Context(), `SELECT value FROM settings WHERE key='telemetry'`).Scan(&raw); err == nil {
			var old snapshot.Telemetry
			if json.Unmarshal(raw, &old) == nil {
				if cfg.Host == "" {
					cfg.Host = old.Host
				}
				if cfg.PublicKey == "" {
					cfg.PublicKey = old.PublicKey
				}
				if cfg.SecretKey == "" {
					cfg.SecretKey = old.SecretKey
				}
			}
		}
	}
	if cfg.Host == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": "host is required"})
		return
	}
	client := &http.Client{Timeout: 8 * time.Second}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		strings.TrimSuffix(cfg.Host, "/")+"/api/public/health", nil)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": err.Error()})
		return
	}
	if cfg.PublicKey != "" {
		req.SetBasicAuth(cfg.PublicKey, cfg.SecretKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": "host unreachable: health check returned " + resp.Status})
		return
	}

	// An empty batch is authenticated before its contents are examined, so this
	// proves the credentials without ingesting a trace into the user's project.
	if cfg.PublicKey == "" || cfg.SecretKey == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": "host reachable, but public/secret key is missing"})
		return
	}
	authReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		strings.TrimSuffix(cfg.Host, "/")+"/api/public/ingestion", strings.NewReader(`{"batch":[]}`))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": err.Error()})
		return
	}
	authReq.Header.Set("Content-Type", "application/json")
	authReq.SetBasicAuth(cfg.PublicKey, cfg.SecretKey)
	authResp, err := client.Do(authReq)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": err.Error()})
		return
	}
	defer authResp.Body.Close()
	switch {
	case authResp.StatusCode == http.StatusUnauthorized || authResp.StatusCode == http.StatusForbidden:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"detail": "host reachable but credentials rejected (" + authResp.Status + ") — check the public/secret key pair belongs to this Langfuse project",
		})
	case authResp.StatusCode >= 300:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": "ingestion probe returned " + authResp.Status})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detail": "reachable and credentials accepted"})
	}
}

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

// traceBaseURL resolves the Langfuse URL prefix for a trace, so the console can
// deep-link a request straight into its trace. The exporter already uses the
// gateway's request id as the Langfuse trace id, so the link is just
// prefix + request_id — no trace lookup or Langfuse query needed.
//
// The project id is not part of the telemetry config, so it is fetched once from
// Langfuse and cached: the host and key pair change about never, and a failure
// here must not break the settings page.
func (m *Server) traceBaseURL(ctx context.Context, cfg snapshot.Telemetry) string {
	if !cfg.Enabled || cfg.Type != "langfuse" || cfg.Host == "" || cfg.PublicKey == "" {
		return ""
	}
	host := strings.TrimSuffix(cfg.Host, "/")
	cacheKey := host + "|" + cfg.PublicKey

	m.traceMu.Lock()
	defer m.traceMu.Unlock()
	if m.traceCache == nil {
		m.traceCache = map[string]string{}
	}
	if v, ok := m.traceCache[cacheKey]; ok {
		return v
	}

	base := "" // negative result is cached too, so a broken host is not retried per request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host+"/api/public/projects", nil)
	if err == nil {
		req.SetBasicAuth(cfg.PublicKey, cfg.SecretKey)
		if resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req); err == nil {
			defer resp.Body.Close()
			var out struct {
				Data []struct{ ID string } `json:"data"`
			}
			if json.NewDecoder(resp.Body).Decode(&out) == nil && len(out.Data) > 0 {
				base = host + "/project/" + out.Data[0].ID + "/traces/"
			}
		}
	}
	m.traceCache[cacheKey] = base
	return base
}
