package mgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/workspace"
)

// Fallbacks used when the connector leaves them unset. Both exist so the
// safety properties hold by default rather than only when configured.
const (
	defaultWorkspaceBudgetUSD  = 5.0
	defaultWorkspaceMaxPerUser = 5
)

// --- connector settings (settings key 'workspaces') ---

func (m *Server) workspaceConfig(ctx context.Context) workspace.Config {
	var raw []byte
	var cfg workspace.Config
	if err := m.Store.Pool.QueryRow(ctx,
		`SELECT value FROM settings WHERE key='workspaces'`).Scan(&raw); err != nil {
		return cfg // absent row = disabled, which is the safe default
	}
	json.Unmarshal(raw, &cfg)
	return cfg
}

func (m *Server) getWorkspaceSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, m.workspaceConfig(r.Context()))
}

func (m *Server) putWorkspaceSettings(w http.ResponseWriter, r *http.Request) {
	cfg, ok := decode[workspace.Config](w, r)
	if !ok {
		return
	}
	if cfg.Enabled && cfg.Socket == "" {
		httpError(w, http.StatusBadRequest, "socket is required when workspaces are enabled")
		return
	}
	if cfg.Runtime == "" {
		cfg.Runtime = "docker"
	}
	if cfg.GatewayURL == "" {
		cfg.GatewayURL = "http://host.containers.internal:8080"
	}
	val, _ := json.Marshal(cfg)
	if _, err := m.Store.Pool.Exec(r.Context(),
		`INSERT INTO settings (key, value) VALUES ('workspaces', $1)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, val); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	m.forgetRuntime() // the IDE proxy must not keep talking to the old socket
	w.WriteHeader(http.StatusNoContent)
}

// --- access ---

// requireWorkspaceAccess gates every workspace route. It fails closed: a
// disabled connector 404s (rather than 403, so a disabled feature is not
// advertised), and an empty allow-list means admins only.
//
// This is permission to use the feature. Reaching a *particular* workspace is
// the separate ownership check in loadWorkspace.
func (m *Server) requireWorkspaceAccess(next http.HandlerFunc) http.Handler {
	return m.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		c := CallerFrom(r.Context())
		teams := make([]string, 0, len(c.TeamRoles))
		for id := range c.TeamRoles {
			teams = append(teams, id)
		}
		if !m.workspaceConfig(r.Context()).Allows(c.IsAdmin, c.UserID, teams) {
			httpError(w, http.StatusNotFound, "workspaces are not available")
			return
		}
		next(w, r)
	})
}

type workspaceRow struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Name        string
	Image       string
	ContainerID string
	APIKeyID    *uuid.UUID
}

// canAccessWorkspace decides who may act on a workspace.
//
// Admin is deliberately NOT a master key here. An admin can list, stop and
// delete a workspace — the containment actions an operator genuinely needs —
// but cannot read its files, exec in it or talk to its agent. Those would be
// acting as the owner inside a live shell, which is an insider-threat surface,
// not a support tool. Ownership is the rule; admin is a narrow exception.
func canAccessWorkspace(c *Caller, ownerID string, adminMayManage bool) bool {
	if c == nil {
		return false
	}
	if c.UserID != "" && ownerID == c.UserID {
		return true
	}
	return adminMayManage && c.IsAdmin
}

// loadWorkspace resolves the id in the path for the OWNER only. This is the
// default on purpose: a handler added later that reaches for the obvious name
// gets the strict check, and forgetting the distinction fails closed rather
// than handing out a shell in someone else's container.
func (m *Server) loadWorkspace(w http.ResponseWriter, r *http.Request) (*workspaceRow, bool) {
	return m.loadWorkspaceScoped(w, r, false)
}

// loadWorkspaceManaged additionally admits global admins, for the containment
// actions only: stop and delete.
func (m *Server) loadWorkspaceManaged(w http.ResponseWriter, r *http.Request) (*workspaceRow, bool) {
	return m.loadWorkspaceScoped(w, r, true)
}

func (m *Server) loadWorkspaceScoped(w http.ResponseWriter, r *http.Request, adminMayManage bool) (*workspaceRow, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return nil, false
	}
	var ws workspaceRow
	err = m.Store.Pool.QueryRow(r.Context(),
		`SELECT id, user_id, name, image, container_id, api_key_id FROM workspaces WHERE id=$1`, id).
		Scan(&ws.ID, &ws.UserID, &ws.Name, &ws.Image, &ws.ContainerID, &ws.APIKeyID)
	if err != nil {
		httpError(w, http.StatusNotFound, "workspace not found")
		return nil, false
	}
	// Same 404 as a missing row: whether someone else's workspace exists is
	// not information a non-owner should be able to probe for.
	if !canAccessWorkspace(CallerFrom(r.Context()), ws.UserID.String(), adminMayManage) {
		httpError(w, http.StatusNotFound, "workspace not found")
		return nil, false
	}
	return &ws, true
}

// runtime builds the configured backend. Handlers never learn which one they
// got — the whole point of the interface.
func (m *Server) runtime(ctx context.Context) (workspace.Runtime, workspace.Config, error) {
	cfg := m.workspaceConfig(ctx)
	rt, err := workspace.New(cfg)
	return rt, cfg, err
}

// sandboxName is the workspace's handle in whichever runtime holds it —
// container name, pod name and volume/PVC name all derive from the id, so
// cleanup never depends on a lookup.
func sandboxName(id uuid.UUID) string { return workspace.Name(id.String()) }

// --- CRUD ---

// listWorkspaces reconciles the stored status against the runtime before
// answering. A container can die, or the daemon can restart, without anyone
// telling us — reporting the stored value would leave the console confidently
// showing "running" next to a container that no longer exists.
func (m *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	const cols = `w.id, w.user_id, w.name, w.image, w.container_id, w.status, w.created_at, u.email AS owner_email`
	sql := `SELECT ` + cols + ` FROM workspaces w JOIN users u ON u.id = w.user_id WHERE w.user_id=$1 ORDER BY w.created_at DESC`
	args := []any{c.UserID}
	if c.IsAdmin {
		// Admins see every workspace for containment; the owner is shown so
		// a row is never mistaken for one's own.
		sql = `SELECT ` + cols + ` FROM workspaces w JOIN users u ON u.id = w.user_id ORDER BY w.created_at DESC`
		args = nil
	}
	rows, err := listRows(r.Context(), m.Store.Pool, sql, args...)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Never let a runtime outage turn into an empty or misleading list: on any
	// error the stored status stands.
	rt, _, rtErr := m.runtime(r.Context())
	for _, row := range rows {
		if rtErr != nil {
			break
		}
		cid, _ := row["container_id"].(string)
		if cid == "" {
			continue
		}
		running, err := rt.Running(r.Context(), cid)
		if err != nil {
			continue
		}
		want := "stopped"
		if running {
			want = "running"
		}
		if row["status"] != want {
			row["status"] = want
			m.Store.Pool.Exec(r.Context(), `UPDATE workspaces SET status=$1 WHERE id=$2`, want, row["id"])
		}
	}
	writeJSON(w, http.StatusOK, rows)
}

func (m *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Name       string `json:"name"`
		Image      string `json:"image"`
		TemplateID string `json:"template_id"`
	}](w, r)
	if !ok {
		return
	}
	c := CallerFrom(r.Context())
	if c.UserID == "" {
		// The bootstrap admin token has no users row to own a workspace.
		httpError(w, http.StatusForbidden, "sign in as a user to create workspaces")
		return
	}
	cfg := m.workspaceConfig(r.Context())
	// A template names the image; the image field alone is for admins who
	// want something that is not on the menu.
	var templateID *uuid.UUID
	if req.TemplateID != "" {
		tid, err := uuid.Parse(req.TemplateID)
		if err != nil {
			httpError(w, http.StatusBadRequest, "invalid template_id")
			return
		}
		if err := m.Store.Pool.QueryRow(r.Context(),
			`SELECT image FROM workspace_templates WHERE id=$1`, tid).Scan(&req.Image); err != nil {
			httpError(w, http.StatusBadRequest, "template not found")
			return
		}
		templateID = &tid
	}
	if req.Image == "" {
		req.Image = cfg.DefaultImage
	}
	if req.Name == "" || req.Image == "" {
		httpError(w, http.StatusBadRequest, "name and a template (or image) are required")
		return
	}
	if !cfg.ImageAllowed(c.IsAdmin, req.Image) && !m.templateImages(r)[req.Image] {
		httpError(w, http.StatusBadRequest, "pick one of the workspace templates")
		return
	}

	// Each workspace is a container and a volume on the host, so unbounded
	// creation is a disk-fill away from taking the host down with it.
	max := cfg.MaxPerUser
	if max <= 0 {
		max = defaultWorkspaceMaxPerUser
	}
	var count int
	if err := m.Store.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM workspaces WHERE user_id=$1`, c.UserID).Scan(&count); err == nil && count >= max {
		httpError(w, http.StatusConflict,
			fmt.Sprintf("you already have %d workspaces (limit %d) — delete one first", count, max))
		return
	}

	id := uuid.New()
	if _, err := m.Store.Pool.Exec(r.Context(),
		`INSERT INTO workspaces (id, user_id, name, image, template_id) VALUES ($1,$2,$3,$4,$5)`,
		id, c.UserID, req.Name, req.Image, templateID); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String()})
}

func (m *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := m.loadWorkspaceManaged(w, r) // containment action: admins too
	if !ok {
		return
	}
	// Best effort: a runtime that is down must not strand the row forever.
	if rt, _, err := m.runtime(r.Context()); err == nil {
		_ = rt.Remove(r.Context(), sandboxName(ws.ID))
	}
	if ws.APIKeyID != nil {
		m.Store.Pool.Exec(r.Context(), `DELETE FROM api_keys WHERE id=$1`, *ws.APIKeyID)
		m.bump(r.Context()) // the key leaves the routing snapshot
	}
	m.Store.Pool.Exec(r.Context(), `DELETE FROM workspaces WHERE id=$1`, ws.ID)
	w.WriteHeader(http.StatusNoContent)
}

// startWorkspace brings the sandbox up, creating it on first use.
//
// The API key is minted on every start rather than at workspace-create time
// because only its hash is stored: we need the plaintext in hand to give the
// runtime, and there is no way to recover it later. The runtime may have to
// recreate the container (a rebuilt template image), so it always gets a
// fresh credential; the previous key is dropped.
func (m *Server) startWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := m.loadWorkspace(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	rt, cfg, err := m.runtime(ctx)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := sandboxName(ws.ID)

	if running, err := rt.Running(ctx, name); err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	} else if running {
		writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
		return
	}

	first := ws.ContainerID == ""
	spec := workspace.Spec{
		WorkspaceID: ws.ID.String(),
		Image:       ws.Image,
		MemoryMB:    cfg.MemoryMB,
		CPUs:        cfg.CPUs,
		DiskGB:      cfg.DiskGB,
	}
	secret, err := m.mintWorkspaceKey(ctx, ws)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	spec.Env = []string{
		"HOME=" + workspace.HomeDir,
		"LLMR_API_KEY=" + secret,
		"LLMR_BASE_URL=" + cfg.GatewayURL,
	}

	if err := rt.Ensure(ctx, spec); err != nil {
		m.Store.Pool.Exec(ctx, `UPDATE workspaces SET status='error' WHERE id=$1`, ws.ID)
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	if first {
		// Once: the file is on the volume and the user's later edits win.
		if err := rt.WriteFiles(ctx, name, []workspace.File{
			{Path: codeServerSettingsFile, Mode: 0o644, Data: []byte(codeServerSettings)},
			{Path: ".home/.zshrc", Mode: 0o644, Data: []byte(zshrc)},
		}); err != nil {
			httpError(w, http.StatusBadGateway, "workspace started but IDE settings failed: "+err.Error())
			return
		}
	}
	ws.ContainerID = name
	m.Store.Pool.Exec(ctx,
		`UPDATE workspaces SET container_id=$1, status='running' WHERE id=$2`, name, ws.ID)

	// Written on every start, not just the first. It needs no secret — the
	// file references $LLMR_API_KEY rather than embedding it — so rewriting is
	// free, it picks up deployments added since, and a workspace whose first
	// start half-failed repairs itself instead of staying permanently unusable.
	if err := m.writePiConfig(ctx, ws, rt, cfg); err != nil {
		httpError(w, http.StatusBadGateway, "workspace started but pi config failed: "+err.Error())
		return
	}
	// The owner's standing files (ssh keys, .gitconfig, tokens) go into $HOME on
	// every start: one upload on the Workspaces page, every workspace has them.
	if files, err := m.userFiles(r, ws.UserID); err != nil {
		httpError(w, http.StatusBadGateway, "workspace started but your files failed: "+err.Error())
		return
	} else if len(files) > 0 {
		if err := rt.WriteFiles(ctx, name, files); err != nil {
			httpError(w, http.StatusBadGateway, "workspace started but your files failed: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

// mintWorkspaceKey issues the gateway key the agent inside the workspace uses,
// so its spend is metered and budget-capped by the machinery every other client
// already goes through. Any previous key is dropped.
func (m *Server) mintWorkspaceKey(ctx context.Context, ws *workspaceRow) (string, error) {
	if ws.APIKeyID != nil {
		m.Store.Pool.Exec(ctx, `DELETE FROM api_keys WHERE id=$1`, *ws.APIKeyID)
	}
	secret, hash, prefix := auth.GenerateKey()
	keyID := uuid.New()
	// A budget is the point of routing the agent through our own gateway. An
	// uncapped key would make the containment story fiction, so a missing or
	// zero setting falls back to a real number rather than to "unlimited".
	budget := m.workspaceConfig(ctx).BudgetUSD
	if budget <= 0 {
		budget = defaultWorkspaceBudgetUSD
	}
	if _, err := m.Store.Pool.Exec(ctx,
		`INSERT INTO api_keys (id, key_hash, key_prefix, name, user_id, budget_usd, budget_period)
		 VALUES ($1,$2,$3,$4,$5,$6,'daily')`,
		keyID, hash[:], prefix, "workspace: "+ws.Name, ws.UserID, budget); err != nil {
		return "", err
	}
	if _, err := m.Store.Pool.Exec(ctx,
		`UPDATE workspaces SET api_key_id=$1 WHERE id=$2`, keyID, ws.ID); err != nil {
		return "", err
	}
	ws.APIKeyID = &keyID
	m.bump(ctx) // the new key has to reach the routing snapshot before pi calls
	return secret, nil
}

// writePiConfig drops pi's provider config on the volume so `pi` works from
// the integrated terminal with no setup. It lives under $HOME,
// which is on the volume too, so it survives the container being recreated.
func (m *Server) writePiConfig(ctx context.Context, ws *workspaceRow, rt workspace.Runtime, cfg workspace.Config) error {
	models := []map[string]string{}
	for _, name := range m.workspaceModels(ws.UserID) {
		models = append(models, map[string]string{"id": name})
	}
	conf := map[string]any{
		"providers": map[string]any{
			"llmr": map[string]any{
				"baseUrl": cfg.GatewayURL + "/v1",
				"api":     "openai-completions",
				"apiKey":  "$LLMR_API_KEY",
				"models":  models,
			},
		},
	}
	body, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return err
	}
	return rt.WriteFiles(ctx, sandboxName(ws.ID), []workspace.File{
		{Path: ".home/.pi/agent/models.json", Data: body},
	})
}

// zshrc is the shell's starting point: history that survives restarts (it
// lives on the volume with the rest of $HOME), completion, a prompt that shows
// where you are. Uploading your own .zshrc on the Workspaces page replaces it.
const zshrc = `# Seeded by llm-router on first start; this file is yours to edit.
HISTFILE=~/.zsh_history HISTSIZE=50000 SAVEHIST=50000
setopt share_history hist_ignore_dups hist_ignore_space autocd
autoload -Uz compinit && compinit -d ~/.zcompdump
zstyle ':completion:*' menu select
bindkey -e
autoload -Uz vcs_info
precmd() { vcs_info }
zstyle ':vcs_info:git:*' formats ' %F{yellow}(%b)%f'
setopt prompt_subst
PROMPT='%F{cyan}%~%f${vcs_info_msg_0_} %F{green}❯%f '
alias ll='ls -lah --color=auto' ls='ls --color=auto'
`

// workspaceModels lists, sorted, the models an agent inside a workspace can
// actually reach. A pass-through deployment forwards the *caller's* upstream
// credential, and an agent in a container has none — offering those would hand
// the user a menu where half the entries fail with a confusing 400.
func (m *Server) workspaceModels(userID uuid.UUID) []string {
	names := []string{}
	snap := m.Snapshots.Get()
	if snap == nil {
		return names
	}
	owner := &auth.Identity{User: snap.UsersByID[userID.String()]}
	for _, name := range snap.ModelNames() {
		if !snap.CallerCredentialOnly(name) && owner.ModelAllowed(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// modelPolicyKey changes whenever any workspace's model list could: the set
// of served models, or any user's model policy.
func (m *Server) modelPolicyKey() string {
	snap := m.Snapshots.Get()
	if snap == nil {
		return ""
	}
	names := snap.ModelNames()
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(strings.Join(names, ","))
	ids := make([]string, 0, len(snap.UsersByID))
	for id := range snap.UsersByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if u := snap.UsersByID[id]; u.AllowedModels != nil {
			allowed := make([]string, 0, len(u.AllowedModels))
			for n := range u.AllowedModels {
				allowed = append(allowed, n)
			}
			sort.Strings(allowed)
			b.WriteString("|" + id + ":" + strings.Join(allowed, ","))
		}
	}
	return b.String()
}

// SyncWorkspaceModels keeps pi's model list in running workspaces in step with
// the routing snapshot, so a model added in the console shows up without a
// workspace restart. Polling the snapshot pointer is free; the file is only
// rewritten when the list actually changed.
func (m *Server) SyncWorkspaceModels(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		key := m.modelPolicyKey()
		if key == last {
			continue
		}
		rows, err := m.Store.Pool.Query(ctx, `SELECT id, user_id, name FROM workspaces WHERE status='running'`)
		if err != nil {
			slog.Error("workspace model sync: list", "err", err)
			continue
		}
		var running []workspaceRow
		for rows.Next() {
			var ws workspaceRow
			if rows.Scan(&ws.ID, &ws.UserID, &ws.Name) == nil {
				running = append(running, ws)
			}
		}
		rows.Close()
		if len(running) == 0 {
			last = key
			continue
		}
		rt, cfg, err := m.runtime(ctx)
		if err != nil {
			slog.Error("workspace model sync: runtime", "err", err)
			continue // retried next tick: last is untouched
		}
		applied := true
		for i := range running {
			if err := m.writePiConfig(ctx, &running[i], rt, cfg); err != nil {
				applied = false
				slog.Warn("workspace model sync: write", "workspace", running[i].ID, "err", err)
			}
		}
		if applied {
			last = key
		}
	}
}

func (m *Server) stopWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := m.loadWorkspaceManaged(w, r) // containment action: admins too
	if !ok {
		return
	}
	rt, _, err := m.runtime(r.Context())
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := rt.Stop(r.Context(), sandboxName(ws.ID)); err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	m.Store.Pool.Exec(r.Context(), `UPDATE workspaces SET status='stopped' WHERE id=$1`, ws.ID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}
