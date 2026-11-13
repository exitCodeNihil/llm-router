// Package mgmt is the management REST API under /api/. M1 authenticates with
// the bootstrap admin token; console sessions + RBAC arrive with the UI (M3).
package mgmt

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exitcodenihil/llm-router/internal/audit"
	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/console"
	"github.com/exitcodenihil/llm-router/internal/gateway"
	"github.com/exitcodenihil/llm-router/internal/metrics"
	"github.com/exitcodenihil/llm-router/internal/provider"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
	"github.com/exitcodenihil/llm-router/internal/workspace"
)

type Server struct {
	Store         *store.Store
	AdminToken    string
	EncryptionKey string

	// Live console surfaces. All optional; nil-safe handlers.
	Snapshots *snapshot.Holder
	// IDEOrigin is the public origin of the separate IDE listener ("" = the
	// IDE is served from this mux, which shares the console's origin).
	IDEOrigin string
	// UsageRetentionMonths is how many months of individual requests are
	// kept (usage.Retention.RawMonths), for the console to say so. 0 = for ever.
	UsageRetentionMonths int

	rtMu  sync.Mutex
	rt    workspace.Runtime
	rtCfg workspace.Config
	rtAt  time.Time
	// TokenGuard limits failed admin-token attempts per client address.
	TokenGuard interface {
		AdminTokenBlocked(http.ResponseWriter, *http.Request) bool
		AdminTokenFailed(*http.Request)
	}
	Live      *metrics.Live
	NodeStore *metrics.NodeStore
	// GatewaySync names the providers an older gateway is not sent.
	GatewaySync interface {
		UnservedProviders(group string, schema int) []string
	}
	HTTP     *metrics.HTTP // the control plane's own listener
	Events   *metrics.Events
	Recorder *telemetry.Recorder
	// Telemetry reports destination health and trace links; nil when no
	// exporter runs in-process.
	Telemetry *telemetry.Manager
	Gateway   *gateway.Server // chat playground bridge into the data plane
	Version   string          // control-plane build version
	Started   time.Time       // control-plane process start
	// Audit records administrative actions; nil records nothing.
	Audit *audit.Recorder
	// TrustedProxies name the client in audit rows (LLMR_TRUSTED_PROXIES).
	TrustedProxies []netip.Prefix
}

func (m *Server) Register(mux *http.ServeMux) { m.routes(mux.Handle) }

// routes hands every management route to handle. Each wrapper authenticates,
// then audits (audit.go), then checks the caller's role.
func (m *Server) routes(handle func(string, http.Handler)) {
	h := func(fn http.HandlerFunc) http.Handler { return m.requireAdmin(fn) }            // global admin
	ha := func(fn http.HandlerFunc) http.Handler { return m.requireAuth(m.audited(fn)) } // any session
	// hw additionally requires that workspaces are enabled and that the caller
	// is in the allow-list; ownership is checked per workspace inside.
	hw := func(fn http.HandlerFunc) http.Handler { return m.requireWorkspaceAccess(fn) }

	handle("GET /api/me", ha(m.me))
	handle("POST /api/me/password", ha(m.changeOwnPassword))
	handle("POST /api/users/{id}/password", h(m.setUserPassword))

	handle("GET /api/users", h(m.listUsers))
	handle("POST /api/users", h(m.createUser))
	handle("PATCH /api/users/{id}", h(m.patchUser))
	handle("DELETE /api/users/{id}", h(m.deleteUser))
	handle("POST /api/users/{id}/erase", h(m.eraseUser))
	handle("GET /api/deleted-users", h(m.listDeletedUsers))

	handle("GET /api/teams", ha(m.listTeamsScoped))
	handle("POST /api/teams", h(m.createTeam))
	handle("PATCH /api/teams/{id}", h(m.patchRow("teams", teamPatchCols)))
	handle("DELETE /api/teams/{id}", h(m.deleteTeam))
	handle("GET /api/teams/{id}/members", ha(m.listTeamMembers))
	handle("POST /api/teams/{id}/members", ha(m.addTeamMember))
	handle("PATCH /api/teams/{id}/members/{uid}", ha(m.patchTeamMember))
	handle("DELETE /api/teams/{id}/members/{uid}", ha(m.removeTeamMember))

	handle("GET /api/keys", ha(m.listKeys))
	handle("POST /api/keys", ha(m.createKey))
	handle("PATCH /api/keys/{id}", ha(m.patchKey))
	handle("DELETE /api/keys/{id}", ha(m.deleteKey))

	handle("GET /api/providers", h(m.listProviders))
	handle("GET /api/providers/{id}/models", h(m.providerModels))
	handle("POST /api/providers", h(m.createProvider))
	handle("PATCH /api/providers/{id}", h(m.patchProvider))
	handle("DELETE /api/providers/{id}", h(m.deleteRow("providers")))

	handle("GET /api/deployments", h(m.listDeployments))
	handle("POST /api/deployments", h(m.createDeployment))
	handle("PATCH /api/deployments/{id}", h(m.patchRow("model_deployments", deploymentPatchCols)))
	handle("DELETE /api/deployments/{id}", h(m.deleteRow("model_deployments")))

	handle("GET /api/prices", h(m.listPrices))
	handle("PUT /api/prices/{id...}", h(m.putPrice))
	handle("POST /api/prices/sync", h(m.syncPrices))

	handle("GET /api/settings/sso", h(m.getSSO))
	handle("PUT /api/settings/sso", h(m.putSSO))
	handle("GET /api/settings/telemetry", h(m.getTelemetry))
	handle("PUT /api/settings/telemetry", h(m.putTelemetry))
	handle("GET /api/settings/workspaces", h(m.getWorkspaceSettings))
	handle("PUT /api/settings/workspaces", h(m.putWorkspaceSettings))

	// Admins manage templates before switching workspaces on, so this list
	// is admin-or-allowed rather than allow-list only.
	handle("GET /api/workspace-templates", ha(func(w http.ResponseWriter, r *http.Request) {
		if CallerFrom(r.Context()).IsAdmin {
			m.listWorkspaceTemplates(w, r)
			return
		}
		m.requireWorkspaceAccess(m.listWorkspaceTemplates).ServeHTTP(w, r)
	}))
	handle("POST /api/workspace-templates", h(m.createWorkspaceTemplate))
	handle("PATCH /api/workspace-templates/{id}", h(m.patchWorkspaceTemplate))
	handle("DELETE /api/workspace-templates/{id}", h(m.deleteRow("workspace_templates")))
	handle("POST /api/workspace-templates/{id}/build", h(m.buildWorkspaceTemplate))
	handle("GET /api/workspaces/files", hw(m.listUserFiles))
	handle("PUT /api/workspaces/files", hw(m.putUserFiles))
	handle("DELETE /api/workspaces/files", hw(m.deleteUserFile))
	handle("GET /api/workspaces", hw(m.listWorkspaces))
	handle("POST /api/workspaces", hw(m.createWorkspace))
	handle("DELETE /api/workspaces/{id}", hw(m.deleteWorkspace))
	handle("POST /api/workspaces/{id}/start", hw(m.startWorkspace))
	handle("POST /api/workspaces/{id}/stop", hw(m.stopWorkspace))
	handle("POST /api/workspaces/{id}/upload", hw(m.uploadFiles))
	// The IDE is a whole application behind one prefix, so it takes every
	// method and every sub-path — including websocket upgrades.
	if m.IDEOrigin == "" {
		handle("/api/workspaces/{id}/ide/", hw(m.workspaceIDE))
	}

	handle("GET /api/routing/health", h(m.routingHealth))
	handle("GET /api/gateway-groups", h(m.listGatewayGroups))
	handle("POST /api/gateway-groups", h(m.createGatewayGroup))
	handle("PATCH /api/gateway-groups/{id}", h(m.patchRow("gateway_groups", gatewayGroupPatchCols)))
	handle("POST /api/gateway-groups/{id}/rotate-token", h(m.rotateGatewayToken))
	handle("DELETE /api/gateway-groups/{id}", h(m.deleteGatewayGroup))
	handle("GET /api/gateway-instances", h(m.listGatewayInstances))
	handle("DELETE /api/gateway-instances/{id}", h(m.deleteRow("gateway_instances")))

	handle("GET /api/token-issuers", h(m.listTokenIssuers))
	handle("POST /api/token-issuers", h(m.createTokenIssuer))
	handle("PATCH /api/token-issuers/{id}", h(m.patchTokenIssuer))
	handle("DELETE /api/token-issuers/{id}", h(m.deleteRow("token_issuers")))

	handle("POST /api/chat/completions", ha(m.chatCompletions))
	handle("GET /api/chat/models", ha(m.chatModels))
	handle("GET /api/models/available", ha(m.availableModels))
	handle("GET /api/chats", ha(m.listChats))
	handle("POST /api/chats", ha(m.createChat))
	handle("GET /api/chats/{id}", ha(m.getChat))
	handle("PUT /api/chats/{id}", ha(m.putChat))
	handle("DELETE /api/chats/{id}", ha(m.deleteChat))

	handle("GET /api/usage/summary", ha(m.usageSummary))
	handle("GET /api/usage/daily", ha(m.usageDaily))
	handle("GET /api/usage/by-model", ha(m.usageByModel))
	handle("GET /api/usage/recent", ha(m.usageRecent))
	handle("GET /api/usage/flow", ha(m.usageFlow))
	handle("GET /api/usage/stats", ha(m.usageStats))
	handle("GET /api/usage/breakdown", ha(m.usageBreakdown))
	handle("GET /api/usage/timeseries", ha(m.usageTimeseries))
	handle("GET /api/usage/requests", ha(m.usageRequests))
	handle("GET /api/usage/by-team", ha(m.usageByTeam))
	handle("GET /api/usage/by-provider-daily", ha(m.usageByProviderDaily))
	handle("GET /api/usage/by-deployment", h(m.usageByDeployment))

	handle("GET /api/metrics/live", h(m.metricsLive))
	handle("GET /api/nodes", h(m.listNodes))
	handle("GET /api/nodes/history", h(m.nodesHistory))

	handle("GET /api/telemetry/kinds", h(m.telemetryKinds))
	handle("GET /api/telemetry/destinations", h(m.listDestinations))
	handle("GET /api/telemetry/destinations/{id}", h(m.getDestination))
	handle("POST /api/telemetry/destinations", h(m.createDestination))
	handle("PUT /api/telemetry/destinations/{id}", h(m.putDestination))
	handle("PATCH /api/telemetry/destinations/{id}", h(m.patchDestination))
	handle("DELETE /api/telemetry/destinations/{id}", h(m.deleteDestination))
	handle("POST /api/telemetry/destinations/test", h(m.testDestination))
	handle("GET /api/observability/exported", h(m.observabilityExported))
	handle("GET /api/observability/rules/stats", h(m.observabilityRulesStats))

	handle("GET /api/audit", h(m.listAudit))
}

func (m *Server) me(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	if c.TeamRoles == nil {
		// nil serializes to JSON null and crashes team_roles[...] lookups in the SPA
		c.TeamRoles = map[string]string{}
	}
	teams := make([]string, 0, len(c.TeamRoles))
	for id := range c.TeamRoles {
		teams = append(teams, id)
	}
	// SSO-provisioned users have no password yet; the console lets them set
	// one without a "current" password only when it knows that.
	hasPassword := false
	if c.UserID != "" {
		m.Store.Pool.QueryRow(r.Context(),
			`SELECT password_hash IS NOT NULL FROM users WHERE id=$1`, c.UserID).Scan(&hasPassword)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":      c.UserID,
		"email":        c.Email,
		"is_admin":     c.IsAdmin,
		"team_roles":   c.TeamRoles,
		"has_password": hasPassword,
		// Whether this caller may use workspaces, so the console can show the
		// feature only to people it works for instead of a failing request.
		"workspaces": m.workspaceConfig(r.Context()).Allows(c.IsAdmin, c.UserID, teams),
		// Where the IDE iframe loads from; "" means this origin.
		"ide_origin": m.IDEOrigin,
		// How long individual requests are kept; 0 = for ever.
		"usage_retention_months": m.UsageRetentionMonths,
	})
}

func (m *Server) requireAdmin(next http.HandlerFunc) http.Handler {
	return m.requireAuth(m.audited(func(w http.ResponseWriter, r *http.Request) {
		if !CallerFrom(r.Context()).IsAdmin {
			httpError(w, http.StatusForbidden, "admin role required")
			return
		}
		next(w, r)
	}))
}

// --- helpers ---

func httpError(w http.ResponseWriter, status int, msg string) {
	if strings.Contains(msg, "SQLSTATE 57014") {
		// The console pool's statement timeout cancelled the query.
		slog.Warn("console query timed out", "err", msg)
		status, msg = http.StatusGatewayTimeout, "the query took too long; try a shorter time range"
	}
	if strings.Contains(msg, "SQLSTATE") {
		// Driver text names tables and constraints. Say what went wrong in the
		// caller's terms and keep the detail in the log.
		slog.Warn("database error", "status", status, "err", msg)
		msg = cleanDBError(msg)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// cleanDBError maps the Postgres error classes an API caller can trigger to
// plain messages. Anything else is "database error": the log has the rest.
func cleanDBError(msg string) string {
	switch {
	case strings.Contains(msg, "SQLSTATE 23505"):
		return "already exists"
	case strings.Contains(msg, "SQLSTATE 23503"):
		return "referenced record does not exist"
	case strings.Contains(msg, "SQLSTATE 23514"), strings.Contains(msg, "SQLSTATE 22P02"), strings.Contains(msg, "SQLSTATE 22"):
		return "invalid value"
	case strings.Contains(msg, "SQLSTATE 23502"):
		return "a required field is missing"
	}
	return "database error"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// emptyToNil treats "" as absent for optional uuid fields — UI forms send
// empty strings, and Postgres rejects "" as a uuid with an opaque SQLSTATE.
func emptyToNil(s *string) *string {
	if s != nil && *s == "" {
		return nil
	}
	return s
}

func decode[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&v); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return v, false
	}
	return v, true
}

func (m *Server) bump(ctx context.Context) {
	if err := m.Store.BumpConfigVersion(ctx); err != nil {
		slog.Error("bump config version failed", "err", err)
	}
}

// reads is the pool for console reads: the one with a statement timeout.
func (m *Server) reads() *pgxpool.Pool {
	if m.Store.Console != nil {
		return m.Store.Console
	}
	return m.Store.Pool
}

// listRows runs a query and returns rows as []map[string]any (column name → value).
func listRows(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) ([]map[string]any, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	fields := rows.FieldDescriptions()
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		rec := map[string]any{}
		for i, f := range fields {
			v := vals[i]
			if u, ok := v.([16]uint8); ok {
				v = uuid.UUID(u).String()
			}
			rec[f.Name] = v
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (m *Server) list(w http.ResponseWriter, r *http.Request, sql string, args ...any) {
	rows, err := listRows(r.Context(), m.reads(), sql, args...)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (m *Server) deleteRow(table string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid id")
			return
		}
		tag, err := m.Store.Pool.Exec(r.Context(), `DELETE FROM `+table+` WHERE id=$1`, id)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if tag.RowsAffected() == 0 {
			httpError(w, http.StatusNotFound, "not found")
			return
		}
		m.bump(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- users ---

func (m *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	m.list(w, r, `SELECT id, email, name, role, budget_usd, budget_period, rpm_limit, tpm_limit, tags, allowed_models, disabled, created_at FROM users ORDER BY created_at`)
}

func (m *Server) createUser(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Email        string   `json:"email"`
		Name         string   `json:"name"`
		Role         string   `json:"role"`
		BudgetUSD    *float64 `json:"budget_usd"`
		BudgetPeriod *string  `json:"budget_period"`
		RPMLimit     *int     `json:"rpm_limit"`
		TPMLimit     *int     `json:"tpm_limit"`
		Tags         []string `json:"tags"`
		// nil = every model; a list is the user's model policy.
		AllowedModels []string `json:"allowed_models"`
		// Optional: without it the user can only sign in through SSO (or an
		// admin sets one later).
		Password string `json:"password"`
	}](w, r)
	if !ok {
		return
	}
	if req.Email == "" {
		httpError(w, http.StatusBadRequest, "email is required")
		return
	}
	if req.Role == "" {
		req.Role = "member"
	}
	var hash []byte // nil stays NULL: no password until one is set
	if req.Password != "" {
		if len(req.Password) < 8 {
			httpError(w, http.StatusBadRequest, "password must be at least 8 characters")
			return
		}
		h, err := console.HashPassword(req.Password)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "hashing failed")
			return
		}
		hash = h
	}
	id := uuid.New()
	_, err := m.Store.Pool.Exec(r.Context(), `
		INSERT INTO users (id, email, name, role, budget_usd, budget_period, rpm_limit, tpm_limit, tags, password_hash, allowed_models)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		id, req.Email, req.Name, req.Role, req.BudgetUSD, req.BudgetPeriod, req.RPMLimit, req.TPMLimit, req.Tags, hash, req.AllowedModels)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditTarget(r, "user", id.String())
	m.bump(r.Context())
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String()})
}

// --- teams ---

// listTeamsScoped: admins see every team, members only their own.
func (m *Server) listTeamsScoped(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	if c.IsAdmin {
		m.list(w, r, `SELECT id, name, budget_usd, budget_period, rpm_limit, tpm_limit, tags, allowed_models, created_at FROM teams ORDER BY created_at`)
		return
	}
	m.list(w, r, `SELECT t.id, t.name, t.budget_usd, t.budget_period, t.rpm_limit, t.tpm_limit, t.tags, t.allowed_models, t.created_at
		FROM teams t JOIN team_members tm ON tm.team_id = t.id WHERE tm.user_id = $1 ORDER BY t.created_at`, c.UserID)
}

func (m *Server) listTeamMembers(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	teamID := r.PathValue("id")
	if !c.InTeam(teamID) {
		httpError(w, http.StatusForbidden, "not a member of this team")
		return
	}
	m.list(w, r, `SELECT tm.user_id, u.email, u.name, tm.role, tm.budget_usd, tm.budget_period FROM team_members tm
		JOIN users u ON u.id = tm.user_id WHERE tm.team_id = $1 ORDER BY u.email`, teamID)
}

// patchTeamMember sets a member's role or their share of the team budget.
// Team admins may do this for their own team; null clears the share.
func (m *Server) patchTeamMember(w http.ResponseWriter, r *http.Request) {
	teamID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid team id")
		return
	}
	userID, err := uuid.Parse(r.PathValue("uid"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	if !CallerFrom(r.Context()).TeamAdmin(teamID.String()) {
		httpError(w, http.StatusForbidden, "team admin role required")
		return
	}
	fields, ok := decodeFields(w, r)
	if !ok {
		return
	}
	set, args := "", []any{teamID, userID}
	for col, raw := range fields {
		switch col {
		case "role", "budget_usd", "budget_period":
		default:
			httpError(w, http.StatusBadRequest, "field not updatable: "+col)
			return
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			httpError(w, http.StatusBadRequest, "invalid value for "+col)
			return
		}
		if f, isNum := v.(float64); col == "budget_usd" && isNum && f < 0 {
			httpError(w, http.StatusBadRequest, "budget_usd cannot be negative")
			return
		}
		args = append(args, v)
		if set != "" {
			set += ", "
		}
		set += col + " = $" + strconv.Itoa(len(args))
	}
	if set == "" {
		httpError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	tag, err := m.Store.Pool.Exec(r.Context(), "UPDATE team_members SET "+set+" WHERE team_id = $1 AND user_id = $2", args...)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpError(w, http.StatusNotFound, "not a member")
		return
	}
	m.bump(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (m *Server) removeTeamMember(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	teamID := r.PathValue("id")
	if !c.TeamAdmin(teamID) {
		httpError(w, http.StatusForbidden, "team admin role required")
		return
	}
	_, err := m.Store.Pool.Exec(r.Context(),
		`DELETE FROM team_members WHERE team_id=$1 AND user_id=$2`, teamID, r.PathValue("uid"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	m.bump(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (m *Server) createTeam(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Name          string   `json:"name"`
		BudgetUSD     *float64 `json:"budget_usd"`
		BudgetPeriod  *string  `json:"budget_period"`
		RPMLimit      *int     `json:"rpm_limit"`
		TPMLimit      *int     `json:"tpm_limit"`
		Tags          []string `json:"tags"`
		AllowedModels []string `json:"allowed_models"`
	}](w, r)
	if !ok {
		return
	}
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	id := uuid.New()
	_, err := m.Store.Pool.Exec(r.Context(), `
		INSERT INTO teams (id, name, budget_usd, budget_period, rpm_limit, tpm_limit, tags, allowed_models)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		id, req.Name, req.BudgetUSD, req.BudgetPeriod, req.RPMLimit, req.TPMLimit, req.Tags, req.AllowedModels)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditTarget(r, "team", id.String())
	m.bump(r.Context())
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String()})
}

func (m *Server) addTeamMember(w http.ResponseWriter, r *http.Request) {
	teamID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid team id")
		return
	}
	if !CallerFrom(r.Context()).TeamAdmin(teamID.String()) {
		httpError(w, http.StatusForbidden, "team admin role required")
		return
	}
	req, ok := decode[struct {
		UserID       string   `json:"user_id"`
		Role         string   `json:"role"`
		BudgetUSD    *float64 `json:"budget_usd"`
		BudgetPeriod *string  `json:"budget_period"`
	}](w, r)
	if !ok {
		return
	}
	if req.Role == "" {
		req.Role = "member"
	}
	if req.BudgetUSD != nil && *req.BudgetUSD < 0 {
		httpError(w, http.StatusBadRequest, "budget_usd cannot be negative")
		return
	}
	if req.UserID == "" {
		httpError(w, http.StatusBadRequest, "user_id is required")
		return
	}
	// Team admins have no user directory; let them name a member by email.
	if _, err := uuid.Parse(req.UserID); err != nil && strings.Contains(req.UserID, "@") {
		if err := m.Store.Pool.QueryRow(r.Context(),
			`SELECT id::text FROM users WHERE email=$1 AND NOT disabled`, strings.TrimSpace(req.UserID)).Scan(&req.UserID); err != nil {
			httpError(w, http.StatusNotFound, "no user with that email")
			return
		}
	}
	auditTarget(r, "team_member", teamID.String()+"/"+req.UserID)
	_, err = m.Store.Pool.Exec(r.Context(), `
		INSERT INTO team_members (team_id, user_id, role, budget_usd, budget_period) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (team_id, user_id) DO UPDATE SET role = EXCLUDED.role,
			budget_usd = COALESCE(EXCLUDED.budget_usd, team_members.budget_usd),
			budget_period = COALESCE(EXCLUDED.budget_period, team_members.budget_period)`,
		teamID, req.UserID, req.Role, req.BudgetUSD, req.BudgetPeriod)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	m.bump(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// --- api keys ---

const keyCols = `id, key_prefix, name, user_id, team_id, allowed_models, budget_usd, budget_period,
	rpm_limit, tpm_limit, expires_at, tags, disabled, created_at`

// listKeys: admins see all keys; others see their own keys and their teams' keys.
func (m *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	if c.IsAdmin {
		m.list(w, r, `SELECT `+keyCols+` FROM api_keys ORDER BY created_at`)
		return
	}
	m.list(w, r, `SELECT `+keyCols+` FROM api_keys
		WHERE user_id = $1 OR team_id = ANY(SELECT team_id FROM team_members WHERE user_id = $1)
		ORDER BY created_at`, c.UserID)
}

// canManageKey: admins manage any key; team admins their team's keys; users their own.
func canManageKey(c *Caller, userID, teamID *string) bool {
	if c.IsAdmin {
		return true
	}
	if teamID != nil && c.TeamAdmin(*teamID) {
		return true
	}
	// Your own key, personal or minted for one of your teams.
	return userID != nil && *userID == c.UserID
}

func (m *Server) createKey(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Name          string     `json:"name"`
		UserID        *string    `json:"user_id"`
		TeamID        *string    `json:"team_id"`
		AllowedModels []string   `json:"allowed_models"`
		BudgetUSD     *float64   `json:"budget_usd"`
		BudgetPeriod  *string    `json:"budget_period"`
		RPMLimit      *int       `json:"rpm_limit"`
		TPMLimit      *int       `json:"tpm_limit"`
		ExpiresAt     *time.Time `json:"expires_at"`
		Tags          []string   `json:"tags"`
	}](w, r)
	if !ok {
		return
	}
	req.UserID, req.TeamID = emptyToNil(req.UserID), emptyToNil(req.TeamID)
	if req.UserID == nil && req.TeamID == nil {
		httpError(w, http.StatusBadRequest, "user_id or team_id is required")
		return
	}
	c := CallerFrom(r.Context())
	if req.TeamID != nil && !c.IsAdmin && !c.TeamAdmin(*req.TeamID) {
		// A member may mint a key for a team they belong to. It is theirs as
		// well as the team's: spend counts against both budgets, and the
		// team's model policy and the member's own both apply.
		if !c.InTeam(*req.TeamID) {
			httpError(w, http.StatusForbidden, "you are not a member of that team")
			return
		}
		me := c.UserID
		req.UserID = &me
	} else if !canManageKey(c, req.UserID, req.TeamID) {
		httpError(w, http.StatusForbidden, "you can only mint keys for yourself or teams you administer")
		return
	}
	if !canSetKeyLimits(CallerFrom(r.Context()), req.TeamID) &&
		(req.BudgetUSD != nil || req.BudgetPeriod != nil || req.RPMLimit != nil || req.TPMLimit != nil) {
		httpError(w, http.StatusForbidden, "budgets and rate limits on keys are set by an admin")
		return
	}
	secret, hash, prefix := auth.GenerateKey()
	id := uuid.New()
	_, err := m.Store.Pool.Exec(r.Context(), `
		INSERT INTO api_keys (id, key_hash, key_prefix, name, user_id, team_id, allowed_models,
			budget_usd, budget_period, rpm_limit, tpm_limit, expires_at, tags)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		id, hash[:], prefix, req.Name, req.UserID, req.TeamID, req.AllowedModels,
		req.BudgetUSD, req.BudgetPeriod, req.RPMLimit, req.TPMLimit, req.ExpiresAt, req.Tags)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditTarget(r, "key", id.String())
	m.bump(r.Context())
	// the secret is returned exactly once and never stored
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String(), "key": secret})
}

// --- providers ---

// validBaseURL accepts an empty value (types with a fixed endpoint) or an
// http(s) URL. Upstream requests carry the provider's credential, so the
// scheme is not something to leave to whoever pastes a value in.
func validBaseURL(v string) bool {
	if v == "" {
		return true
	}
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (m *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	m.list(w, r, `SELECT id, name, type, base_url, auth_mode, (api_key_enc IS NOT NULL) AS has_api_key, config, created_at FROM providers ORDER BY created_at`)
}

func (m *Server) createProvider(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Name     string         `json:"name"`
		Type     string         `json:"type"`
		BaseURL  string         `json:"base_url"`
		AuthMode string         `json:"auth_mode"`
		APIKey   string         `json:"api_key"`
		Config   map[string]any `json:"config"`
	}](w, r)
	if !ok {
		return
	}
	if req.Name == "" || req.Type == "" || req.AuthMode == "" {
		httpError(w, http.StatusBadRequest, "name, type, auth_mode are required")
		return
	}
	// Vertex derives its endpoint from the location and OpenRouter has one
	// address, so base_url is optional there — everywhere else there is
	// nothing to route to without it.
	if !validBaseURL(req.BaseURL) {
		httpError(w, http.StatusBadRequest, "base_url must start with http:// or https://")
		return
	}
	if req.BaseURL == "" && req.Type != "gcp_vertex" && req.Type != "openrouter" && req.Type != "anthropic" {
		httpError(w, http.StatusBadRequest, "base_url is required")
		return
	}
	var enc []byte
	if req.APIKey != "" {
		var err error
		enc, err = snapshot.Encrypt(m.EncryptionKey, req.APIKey)
		if err != nil {
			httpError(w, http.StatusBadRequest, "cannot store api key: "+err.Error())
			return
		}
	}
	if req.Config == nil {
		req.Config = map[string]any{}
	}
	if req.Type == "gcp_vertex" {
		if err := validateVertexConfig(req.AuthMode, req.Config); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.AuthMode == "anthropic_wif" {
		if err := provider.ValidateAnthropicWIF(snapshot.AnthropicWIFFromConfig(req.Config)); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	id := uuid.New()
	_, err := m.Store.Pool.Exec(r.Context(), `
		INSERT INTO providers (id, name, type, base_url, auth_mode, api_key_enc, config)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, req.Name, req.Type, req.BaseURL, req.AuthMode, enc, req.Config)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditTarget(r, "provider", id.String())
	m.bump(r.Context())
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String()})
}

func validateVertexConfig(authMode string, cfg map[string]any) error {
	project, _ := cfg["project"].(string)
	location, _ := cfg["location"].(string)
	return provider.ValidateVertex(authMode, project, location)
}

// routingHealth lists deployments the gateway is currently holding out of
// routing after failures, so the Models page can show why traffic moved.
func (m *Server) routingHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, gateway.CoolingDeployments())
}

// providerModels asks the upstream provider what models/deployments it offers.
func (m *Server) providerModels(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	p := &snapshot.Provider{ID: id.String()}
	var enc []byte
	var cfg map[string]any
	err = m.Store.Pool.QueryRow(r.Context(),
		`SELECT name, type, base_url, auth_mode, api_key_enc, config FROM providers WHERE id=$1`, id).
		Scan(&p.Name, &p.Type, &p.BaseURL, &p.AuthMode, &enc, &cfg)
	if err != nil {
		httpError(w, http.StatusNotFound, "provider not found")
		return
	}
	p.ApplyConfig(cfg)
	if len(enc) > 0 && snapshot.ProviderHasSecret(p.AuthMode) {
		if p.APIKey, err = snapshot.Decrypt(m.EncryptionKey, enc); err != nil {
			httpError(w, http.StatusInternalServerError,
				"cannot decrypt provider credentials — LLMR_ENCRYPTION_KEY changed since the key was saved; re-enter the provider API key")
			return
		}
	}
	models, err := provider.ListModels(r.Context(), p)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	if p.Type == "openrouter" {
		// Discovery is the moment we hold live prices; keep the catalog current
		// so models referencing openrouter/<slug> follow OpenRouter's rates.
		if err := m.syncCatalog(r.Context(), "openrouter", models); err != nil {
			slog.Warn("openrouter price sync failed", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, models)
}

// syncCatalog upserts <prefix>/<id> catalog rows from discovery results that
// carry prices. Admin-edited rows are left alone, like the shipped seed.
func (m *Server) syncCatalog(ctx context.Context, prefix string, models []provider.ModelInfo) error {
	n := 0
	for _, mi := range models {
		if mi.InputPer1M == nil || mi.OutputPer1M == nil {
			continue
		}
		if _, err := m.Store.Pool.Exec(ctx, `
			INSERT INTO price_catalog (model_id, input_per_1m, output_per_1m, cached_input_per_1m, source, updated_at)
			VALUES ($1, $2, $3, $4, 'seed', now())
			ON CONFLICT (model_id) DO UPDATE
			SET input_per_1m = EXCLUDED.input_per_1m,
			    output_per_1m = EXCLUDED.output_per_1m,
			    cached_input_per_1m = EXCLUDED.cached_input_per_1m,
			    updated_at = now()
			WHERE price_catalog.source = 'seed'
			  AND (price_catalog.input_per_1m, price_catalog.output_per_1m) IS DISTINCT FROM (EXCLUDED.input_per_1m, EXCLUDED.output_per_1m)`,
			prefix+"/"+mi.ID, *mi.InputPer1M, *mi.OutputPer1M, mi.CachedInputPer1M); err != nil {
			return err
		}
		n++
	}
	if n > 0 {
		m.bump(ctx)
	}
	return nil
}

// syncPrices refreshes the catalog from every provider that publishes prices
// (OpenRouter today). Returns how many providers were synced.
func (m *Server) syncPrices(w http.ResponseWriter, r *http.Request) {
	rows, err := m.Store.Pool.Query(r.Context(),
		`SELECT id, name, type, base_url, auth_mode, api_key_enc, config FROM providers WHERE type='openrouter'`)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var ps []*snapshot.Provider
	for rows.Next() {
		p := &snapshot.Provider{}
		var enc []byte
		var cfg map[string]any
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &p.AuthMode, &enc, &cfg); err != nil {
			rows.Close()
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(enc) > 0 && snapshot.ProviderHasSecret(p.AuthMode) {
			if p.APIKey, err = snapshot.Decrypt(m.EncryptionKey, enc); err != nil {
				rows.Close()
				httpError(w, http.StatusInternalServerError, "cannot decrypt provider credentials for "+p.Name)
				return
			}
		}
		ps = append(ps, p)
	}
	rows.Close()
	synced, models := 0, 0
	for _, p := range ps {
		list, err := provider.ListModels(r.Context(), p)
		if err != nil {
			httpError(w, http.StatusBadGateway, p.Name+": "+err.Error())
			return
		}
		if err := m.syncCatalog(r.Context(), "openrouter", list); err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		synced++
		models += len(list)
	}
	writeJSON(w, http.StatusOK, map[string]int{"providers": synced, "models": models})
}

// --- deployments ---

func (m *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	m.list(w, r, `SELECT d.id, d.provider_id, p.name AS provider_name, d.model_name, d.upstream_name, d.api_flavor,
		d.priority, d.catalog_model_id, d.input_per_1m, d.output_per_1m, d.cached_input_per_1m, d.context_tokens, d.region, d.timeout_ms,
		d.enabled, d.created_at
		FROM model_deployments d JOIN providers p ON p.id = d.provider_id ORDER BY d.model_name, d.priority`)
}

func (m *Server) createDeployment(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		ProviderID       string   `json:"provider_id"`
		ModelName        string   `json:"model_name"`
		UpstreamName     string   `json:"upstream_name"`
		APIFlavor        string   `json:"api_flavor"`
		Priority         int      `json:"priority"`
		CatalogModelID   *string  `json:"catalog_model_id"`
		InputPer1M       *float64 `json:"input_per_1m"`
		OutputPer1M      *float64 `json:"output_per_1m"`
		CachedInputPer1M *float64 `json:"cached_input_per_1m"`
		ContextTokens    *int     `json:"context_tokens"`
		Region           string   `json:"region"`
		TimeoutMS        *int     `json:"timeout_ms"`
	}](w, r)
	if !ok {
		return
	}
	if req.ProviderID == "" || req.ModelName == "" || req.UpstreamName == "" {
		httpError(w, http.StatusBadRequest, "provider_id, model_name, upstream_name are required")
		return
	}
	if req.APIFlavor == "" {
		// Claude models on Azure AI Foundry speak the Anthropic Messages
		// protocol; default the flavor so discovery-picked deployments just work
		req.APIFlavor = "openai"
		if strings.HasPrefix(strings.ToLower(req.UpstreamName), "claude") {
			req.APIFlavor = "anthropic"
		}
	}
	id := uuid.New()
	_, err := m.Store.Pool.Exec(r.Context(), `
		INSERT INTO model_deployments (id, provider_id, model_name, upstream_name, api_flavor, priority, catalog_model_id, input_per_1m, output_per_1m, cached_input_per_1m, context_tokens, region, timeout_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		id, req.ProviderID, req.ModelName, req.UpstreamName, req.APIFlavor, req.Priority, req.CatalogModelID, req.InputPer1M, req.OutputPer1M, req.CachedInputPer1M, req.ContextTokens, req.Region, req.TimeoutMS)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditTarget(r, "deployment", id.String())
	m.bump(r.Context())
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String()})
}

// --- prices ---

func (m *Server) listPrices(w http.ResponseWriter, r *http.Request) {
	m.list(w, r, `SELECT model_id, input_per_1m, output_per_1m, cached_input_per_1m, source, updated_at FROM price_catalog ORDER BY model_id`)
}

func (m *Server) putPrice(w http.ResponseWriter, r *http.Request) {
	modelID := r.PathValue("id")
	req, ok := decode[struct {
		InputPer1M       float64  `json:"input_per_1m"`
		OutputPer1M      float64  `json:"output_per_1m"`
		CachedInputPer1M *float64 `json:"cached_input_per_1m"`
	}](w, r)
	if !ok {
		return
	}
	_, err := m.Store.Pool.Exec(r.Context(), `
		INSERT INTO price_catalog (model_id, input_per_1m, output_per_1m, cached_input_per_1m, source, updated_at)
		VALUES ($1,$2,$3,$4,'admin',now())
		ON CONFLICT (model_id) DO UPDATE
		SET input_per_1m=EXCLUDED.input_per_1m, output_per_1m=EXCLUDED.output_per_1m,
		    cached_input_per_1m=EXCLUDED.cached_input_per_1m, source='admin', updated_at=now()`,
		modelID, req.InputPer1M, req.OutputPer1M, req.CachedInputPer1M)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	m.bump(r.Context())
	w.WriteHeader(http.StatusNoContent)
}
