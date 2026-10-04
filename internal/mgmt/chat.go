package mgmt

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/auth"
)

// --- chat playground ---
//
// The console chat bridges the signed-in session user straight into the
// gateway pipeline (no API key): usage is attributed to the user and per-user
// budgets/rate limits apply. Saved conversations live in the chats table,
// strictly owner-scoped.

// chatCompletions proxies a chat request for the session user through the
// gateway's normal routing/failover/usage path, streaming included.
func (m *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if m.Gateway == nil {
		httpError(w, http.StatusServiceUnavailable, "chat is not available on this node")
		return
	}
	c := CallerFrom(r.Context())
	id := &auth.Identity{}
	if snap := m.Snapshots.Get(); snap != nil && c.UserID != "" {
		id.User = snap.UsersByID[c.UserID]
	}
	// ponytail: bootstrap-admin (token, no user row) chats as an anonymous
	// unlimited identity — usage rows carry no user_id.
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/v1/chat/completions"
	m.Gateway.ServeAuthed(w, r2, id)
}

// chatModels lists the public model aliases for the picker. Session-scoped
// (unlike admin-only /api/deployments) and key-agnostic (unlike /v1/models).
// The playground has no upstream token of its own, so pass-through-only
// models are left out.
func (m *Server) chatModels(w http.ResponseWriter, r *http.Request) {
	names := []string{}
	if snap := m.Snapshots.Get(); snap != nil {
		id := &auth.Identity{User: snap.UsersByID[CallerFrom(r.Context()).UserID]}
		for _, name := range snap.ModelNames() {
			if !snap.CallerCredentialOnly(name) && id.ModelAllowed(name) {
				names = append(names, name)
			}
		}
	}
	writeJSON(w, http.StatusOK, names)
}

// requireChatUser: saved chats need a real user row (the bootstrap token has
// none). Returns the caller's user id or writes a 403.
func requireChatUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	c := CallerFrom(r.Context())
	if c.UserID == "" {
		httpError(w, http.StatusForbidden, "sign in as a user to save chats")
		return "", false
	}
	return c.UserID, true
}

func (m *Server) listChats(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireChatUser(w, r)
	if !ok {
		return
	}
	m.list(w, r, `SELECT id, title, model, updated_at FROM chats
		WHERE user_id = $1 ORDER BY updated_at DESC`, userID)
}

func (m *Server) createChat(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireChatUser(w, r)
	if !ok {
		return
	}
	req, ok := decode[struct {
		Title string `json:"title"`
		Model string `json:"model"`
	}](w, r)
	if !ok {
		return
	}
	if req.Title == "" {
		req.Title = "New chat"
	}
	id := uuid.New()
	_, err := m.Store.Pool.Exec(r.Context(),
		`INSERT INTO chats (id, user_id, title, model) VALUES ($1,$2,$3,$4)`,
		id, userID, req.Title, req.Model)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String()})
}

func (m *Server) getChat(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireChatUser(w, r)
	if !ok {
		return
	}
	rows, err := listRows(r.Context(), m.Store.Pool, `
		SELECT id, title, model, system_prompt, settings, messages, created_at, updated_at
		FROM chats WHERE id = $1 AND user_id = $2`, r.PathValue("id"), userID)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(rows) == 0 {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, rows[0])
}

func (m *Server) putChat(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireChatUser(w, r)
	if !ok {
		return
	}
	req, ok := decode[struct {
		Title        *string         `json:"title"`
		Model        *string         `json:"model"`
		SystemPrompt *string         `json:"system_prompt"`
		Settings     json.RawMessage `json:"settings"`
		Messages     json.RawMessage `json:"messages"`
	}](w, r)
	if !ok {
		return
	}
	tag, err := m.Store.Pool.Exec(r.Context(), `
		UPDATE chats SET
			title         = COALESCE($3, title),
			model         = COALESCE($4, model),
			system_prompt = COALESCE($5, system_prompt),
			settings      = COALESCE($6, settings),
			messages      = COALESCE($7, messages),
			updated_at    = now()
		WHERE id = $1 AND user_id = $2`,
		r.PathValue("id"), userID, req.Title, req.Model, req.SystemPrompt, req.Settings, req.Messages)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Server) deleteChat(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireChatUser(w, r)
	if !ok {
		return
	}
	tag, err := m.Store.Pool.Exec(r.Context(),
		`DELETE FROM chats WHERE id = $1 AND user_id = $2`, r.PathValue("id"), userID)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
