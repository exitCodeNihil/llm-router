package mgmt

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/console"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// --- generic PATCH ---

var (
	userPatchCols       = []string{"name", "role", "budget_usd", "budget_period", "rpm_limit", "tpm_limit", "disabled", "tags", "allowed_models"}
	teamPatchCols       = []string{"name", "budget_usd", "budget_period", "rpm_limit", "tpm_limit", "tags", "allowed_models"}
	keyPatchCols        = []string{"name", "allowed_models", "budget_usd", "budget_period", "rpm_limit", "tpm_limit", "expires_at", "disabled", "tags"}
	deploymentPatchCols = []string{"provider_id", "model_name", "upstream_name", "api_flavor", "priority", "catalog_model_id", "input_per_1m", "output_per_1m", "cached_input_per_1m", "context_tokens", "enabled"}
	providerPatchCols   = []string{"name", "base_url", "auth_mode", "config"}
)

// patchRow updates whitelisted columns from the request JSON. Unknown fields 400.
func (m *Server) patchRow(table string, allowed []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid id")
			return
		}
		fields, ok := decodeFields(w, r)
		if !ok || !m.applyPatch(w, r, table, id, allowed, fields) {
			return
		}
		m.bump(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}
}

func decodeFields(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&fields); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON")
		return nil, false
	}
	return fields, true
}

func (m *Server) applyPatch(w http.ResponseWriter, r *http.Request, table string, id uuid.UUID, allowed []string, fields map[string]json.RawMessage) bool {
	if len(fields) == 0 {
		httpError(w, http.StatusBadRequest, "no fields to update")
		return false
	}
	set := ""
	args := []any{id}
	for col, raw := range fields {
		ok := false
		for _, a := range allowed {
			if col == a {
				ok = true
				break
			}
		}
		if !ok {
			httpError(w, http.StatusBadRequest, "field not updatable: "+col)
			return false
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			httpError(w, http.StatusBadRequest, "invalid value for "+col)
			return false
		}
		args = append(args, v)
		if set != "" {
			set += ", "
		}
		set += col + " = $" + strconv.Itoa(len(args))
	}
	tag, err := m.Store.Pool.Exec(r.Context(),
		"UPDATE "+table+" SET "+set+" WHERE id = $1", args...)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if tag.RowsAffected() == 0 {
		httpError(w, http.StatusNotFound, "not found")
		return false
	}
	return true
}

// --- key patch/delete with ownership checks ---

// keyOwnership resolves the key in the path and checks the caller may manage
// it. Someone else's key answers 404, like a missing one: the id space must
// not be probeable.
func (m *Server) keyOwnership(w http.ResponseWriter, r *http.Request) (id uuid.UUID, teamID *string, ok bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return id, nil, false
	}
	var userID *string
	if err := m.Store.Pool.QueryRow(r.Context(),
		`SELECT user_id::text, team_id::text FROM api_keys WHERE id=$1`, id).Scan(&userID, &teamID); err != nil {
		httpError(w, http.StatusNotFound, "not found")
		return id, nil, false
	}
	if !canManageKey(CallerFrom(r.Context()), userID, teamID) {
		httpError(w, http.StatusNotFound, "not found")
		return id, nil, false
	}
	return id, teamID, true
}

// limitFields are the spend controls on a key. Only an admin, or the admin of
// the team the key belongs to, may set them: a member choosing their own cap
// is no cap at all.
var limitFields = []string{"budget_usd", "budget_period", "rpm_limit", "tpm_limit"}

func canSetKeyLimits(c *Caller, teamID *string) bool {
	return c.IsAdmin || (teamID != nil && c.TeamAdmin(*teamID))
}

func (m *Server) patchKey(w http.ResponseWriter, r *http.Request) {
	id, teamID, ok := m.keyOwnership(w, r)
	if !ok {
		return
	}
	fields, ok := decodeFields(w, r)
	if !ok {
		return
	}
	if !canSetKeyLimits(CallerFrom(r.Context()), teamID) {
		for _, f := range limitFields {
			if _, present := fields[f]; present {
				httpError(w, http.StatusForbidden, "budgets and rate limits on keys are set by an admin")
				return
			}
		}
	}
	if !m.applyPatch(w, r, "api_keys", id, keyPatchCols, fields) {
		return
	}
	m.bump(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (m *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, _, ok := m.keyOwnership(w, r)
	if !ok {
		return
	}
	if _, err := m.Store.Pool.Exec(r.Context(), `DELETE FROM api_keys WHERE id=$1`, id); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	m.bump(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// --- provider patch (api_key needs encryption) ---

func (m *Server) patchProvider(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	fields, ok := decodeFields(w, r)
	if !ok {
		return
	}
	if raw, ok := fields["base_url"]; ok {
		// The stored secret was entered for the endpoint it points at. A
		// repointed base_url with the old credential would hand that
		// credential to whatever host the URL now names; make the operator
		// re-enter it deliberately. Runs before the api_key branch so a key
		// sent in the same request survives.
		var base string
		if err := json.Unmarshal(raw, &base); err != nil || !validBaseURL(base) {
			httpError(w, http.StatusBadRequest, "base_url must start with http:// or https://")
			return
		}
		if _, err := m.Store.Pool.Exec(r.Context(),
			`UPDATE providers SET api_key_enc=NULL WHERE id=$1 AND base_url<>$2 AND auth_mode NOT IN ('gcp_adc','entra','none')`, id, base); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if raw, ok := fields["api_key"]; ok {
		var apiKey string
		if err := json.Unmarshal(raw, &apiKey); err != nil {
			httpError(w, http.StatusBadRequest, "invalid api_key")
			return
		}
		enc, err := snapshot.Encrypt(m.EncryptionKey, apiKey)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := m.Store.Pool.Exec(r.Context(),
			`UPDATE providers SET api_key_enc=$2 WHERE id=$1`, id, enc); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		delete(fields, "api_key")
	} else if raw, ok := fields["auth_mode"]; ok {
		// A secret belongs to the mode it was entered for. Kept across a mode
		// change it is fed to the wrong parser at best, and at worst a service
		// account JSON — private key included — is sent upstream as an API key.
		var mode string
		if err := json.Unmarshal(raw, &mode); err != nil {
			httpError(w, http.StatusBadRequest, "invalid auth_mode")
			return
		}
		if _, err := m.Store.Pool.Exec(r.Context(),
			`UPDATE providers SET api_key_enc=NULL WHERE id=$1 AND auth_mode<>$2`, id, mode); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if len(fields) > 0 {
		if !m.vertexPatchValid(w, r, id, fields) {
			return
		}
		if !m.applyPatch(w, r, "providers", id, providerPatchCols, fields) {
			return
		}
	}
	m.bump(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// vertexPatchValid checks the row a patch would produce, so a Vertex provider
// cannot be edited into one the gateway answers 500 for on every request.
func (m *Server) vertexPatchValid(w http.ResponseWriter, r *http.Request, id uuid.UUID, fields map[string]json.RawMessage) bool {
	_, modeChange := fields["auth_mode"]
	_, cfgChange := fields["config"]
	if !modeChange && !cfgChange {
		return true
	}
	var typ, mode string
	var cfg map[string]any
	if err := m.Store.Pool.QueryRow(r.Context(),
		`SELECT type, auth_mode, config FROM providers WHERE id=$1`, id).Scan(&typ, &mode, &cfg); err != nil {
		httpError(w, http.StatusNotFound, "provider not found")
		return false
	}
	if typ != "gcp_vertex" {
		return true
	}
	if raw, ok := fields["auth_mode"]; ok {
		json.Unmarshal(raw, &mode)
	}
	if raw, ok := fields["config"]; ok {
		json.Unmarshal(raw, &cfg)
	}
	if err := validateVertexConfig(mode, cfg); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

// --- SSO settings ---

func (m *Server) getSSO(w http.ResponseWriter, r *http.Request) {
	var raw []byte
	err := m.Store.Pool.QueryRow(r.Context(), `SELECT value FROM settings WHERE key='sso'`).Scan(&raw)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	var cfg console.SSOConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		httpError(w, http.StatusInternalServerError, "corrupt sso config")
		return
	}
	cfg.ClientSecret = "" // never round-trip the secret
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "sso": cfg})
}

func (m *Server) putSSO(w http.ResponseWriter, r *http.Request) {
	cfg, ok := decode[console.SSOConfig](w, r)
	if !ok {
		return
	}
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		httpError(w, http.StatusBadRequest, "issuer_url, client_id, redirect_url are required")
		return
	}
	if cfg.ClientSecret == "" {
		// keep the existing secret on update
		var raw []byte
		if err := m.Store.Pool.QueryRow(r.Context(), `SELECT value FROM settings WHERE key='sso'`).Scan(&raw); err == nil {
			var old console.SSOConfig
			if json.Unmarshal(raw, &old) == nil {
				cfg.ClientSecret = old.ClientSecret
			}
		}
	}
	val, _ := json.Marshal(cfg)
	if _, err := m.Store.Pool.Exec(r.Context(), `
		INSERT INTO settings (key, value) VALUES ('sso', $1)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, val); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- telemetry settings ---

func (m *Server) getTelemetry(w http.ResponseWriter, r *http.Request) {
	var raw []byte
	err := m.Store.Pool.QueryRow(r.Context(), `SELECT value FROM settings WHERE key='telemetry'`).Scan(&raw)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	var cfg snapshot.Telemetry
	if err := json.Unmarshal(raw, &cfg); err != nil {
		httpError(w, http.StatusInternalServerError, "corrupt telemetry config")
		return
	}
	// Resolved before the secret is cleared: the project lookup authenticates
	// with it, and blanking first left the link silently unavailable.
	traceBase := m.traceBaseURL(r.Context(), cfg)
	cfg.SecretKey = "" // never round-trip the secret
	out := map[string]any{"configured": true, "telemetry": cfg}
	if traceBase != "" {
		out["trace_base_url"] = traceBase
	}
	if m.Exporter != nil {
		out["health"] = m.Exporter.Health()
	}
	writeJSON(w, http.StatusOK, out)
}

func (m *Server) putTelemetry(w http.ResponseWriter, r *http.Request) {
	cfg, ok := decode[snapshot.Telemetry](w, r)
	if !ok {
		return
	}
	if cfg.Type == "" {
		cfg.Type = "langfuse"
	}
	if cfg.Enabled && (cfg.Host == "" || cfg.PublicKey == "") {
		httpError(w, http.StatusBadRequest, "host and public_key are required when enabled")
		return
	}
	if cfg.SecretKey == "" {
		var raw []byte
		if err := m.Store.Pool.QueryRow(r.Context(), `SELECT value FROM settings WHERE key='telemetry'`).Scan(&raw); err == nil {
			var old snapshot.Telemetry
			if json.Unmarshal(raw, &old) == nil {
				cfg.SecretKey = old.SecretKey
			}
		}
	}
	val, _ := json.Marshal(cfg)
	if _, err := m.Store.Pool.Exec(r.Context(), `
		INSERT INTO settings (key, value) VALUES ('telemetry', $1)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, val); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	m.bump(r.Context()) // snapshot reload applies the exporter config live
	w.WriteHeader(http.StatusNoContent)
}

// --- usage dashboards ---

// usageScope returns a WHERE fragment limiting rows to what the caller may see.
func usageScope(c *Caller, args *[]any) string {
	if c.IsAdmin {
		return "TRUE"
	}
	*args = append(*args, c.UserID)
	n := strconv.Itoa(len(*args))
	return `(user_id = $` + n + ` OR team_id = ANY(SELECT team_id FROM team_members WHERE user_id = $` + n + `))`
}

func daysParam(r *http.Request) int {
	d, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil || d < 1 || d > 365 {
		return 30
	}
	return d
}

func (m *Server) usageSummary(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	f := newUsageFilter(r, c)
	if f.Err != nil {
		httpError(w, http.StatusBadRequest, f.Err.Error())
		return
	}
	m.listOne(w, r, fmt.Sprintf(`
		SELECT count(*) AS requests,
		       coalesce(sum(prompt_tokens + completion_tokens), 0) AS tokens,
		       coalesce(sum(prompt_tokens), 0) AS prompt_tokens,
		       coalesce(sum(cached_tokens), 0) AS cached_tokens,
		       coalesce(sum(cost_usd), 0) AS cost_usd,
		       count(*) FILTER (WHERE unpriced) AS unpriced_requests,
		       count(*) FILTER (WHERE status_code >= 400) AS errors
		FROM %s WHERE %s`, f.from(), f.where()), f.args...)
}

func (m *Server) usageDaily(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	args := []any{daysParam(r)}
	scope := usageScope(c, &args)
	m.list(w, r, fmt.Sprintf(`
		SELECT date_trunc('day', ts)::date AS day,
		       count(*) AS requests,
		       coalesce(sum(prompt_tokens + completion_tokens), 0) AS tokens,
		       coalesce(sum(cost_usd), 0) AS cost_usd
		FROM usage_events WHERE ts >= now() - make_interval(days => $1) AND %s
		GROUP BY 1 ORDER BY 1`, scope), args...)
}

func (m *Server) usageByModel(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	f := newUsageFilter(r, c)
	if f.Err != nil {
		httpError(w, http.StatusBadRequest, f.Err.Error())
		return
	}
	m.list(w, r, fmt.Sprintf(`
		SELECT model_name,
		       count(*) AS requests,
		       coalesce(sum(prompt_tokens), 0) AS prompt_tokens,
		       coalesce(sum(completion_tokens), 0) AS completion_tokens,
		       coalesce(sum(cost_usd), 0) AS cost_usd
		FROM %s WHERE %s
		GROUP BY 1 ORDER BY cost_usd DESC`, f.from(), f.where()), f.args...)
}

// usageFlow returns key → alias → provider triples over the selected range, so
// the routing diagram can follow the dashboard's time range. The live metrics it
// used before only ever held a rolling minute, which read as "no traffic" on a
// page headed "last 90 days".
func (m *Server) usageFlow(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	args := []any{daysParam(r)}
	scope := usageScope(c, &args)
	// Aggregate before joining: the scope fragment references bare user_id and
	// team_id, which api_keys would make ambiguous.
	m.list(w, r, fmt.Sprintf(`
		WITH f AS (
			SELECT api_key_id, model_name, provider_id, count(*) AS volume
			FROM usage_events
			WHERE ts >= now() - make_interval(days => $1)
			  AND provider_id IS NOT NULL AND %s
			GROUP BY 1, 2, 3
			ORDER BY volume DESC
			LIMIT 200
		)
		SELECT f.api_key_id,
		       coalesce(k.name, '(deleted key)') AS key_name,
		       f.model_name,
		       f.provider_id,
		       coalesce(p.name, '(deleted provider)') AS provider_name,
		       f.volume
		FROM f
		LEFT JOIN api_keys  k ON k.id = f.api_key_id
		LEFT JOIN providers p ON p.id = f.provider_id
		ORDER BY f.volume DESC`, scope), args...)
}

func (m *Server) usageRecent(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit < 1 || limit > 500 {
		limit = 50
	}
	args := []any{limit}
	scope := usageScope(c, &args)
	m.list(w, r, fmt.Sprintf(`
		SELECT ts, request_id, model_name, api_key_id, user_id, team_id,
		       prompt_tokens, completion_tokens, cost_usd, latency_ms, status_code, stream
		FROM usage_events WHERE %s ORDER BY ts DESC LIMIT $1`, scope), args...)
}

// listOne is list() but returns a single object instead of an array.
func (m *Server) listOne(w http.ResponseWriter, r *http.Request, sql string, args ...any) {
	rows, err := listRows(r.Context(), m.Store.Pool, sql, args...)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(rows) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, rows[0])
}
