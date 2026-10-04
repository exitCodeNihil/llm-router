package mgmt

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// lastAdmin reports whether id is the only enabled admin. Removing that
// account (delete, disable, demote) locks everyone out of administration,
// with the first-run setup screen as the only way back in.
func (m *Server) lastAdmin(r *http.Request, id uuid.UUID) bool {
	var n int
	var isAdmin bool
	m.Store.Pool.QueryRow(r.Context(),
		`SELECT count(*), bool_or(id=$1) FROM users WHERE role='admin' AND NOT disabled`, id).Scan(&n, &isAdmin)
	return isAdmin && n == 1
}

func (m *Server) patchUser(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	fields, ok := decodeFields(w, r)
	if !ok {
		return
	}
	demote, disable := false, false
	if raw, ok := fields["role"]; ok {
		var role string
		demote = json.Unmarshal(raw, &role) == nil && role != "admin"
	}
	if raw, ok := fields["disabled"]; ok {
		json.Unmarshal(raw, &disable)
	}
	if (demote || disable) && m.lastAdmin(r, id) {
		httpError(w, http.StatusConflict, "this is the only admin account; make someone else an admin first")
		return
	}
	if !m.applyPatch(w, r, "users", id, userPatchCols, fields) {
		return
	}
	m.bump(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// deleteUser removes the account and everything the database cascade cannot
// reach: the containers and volumes behind the user's workspaces.
func (m *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if m.lastAdmin(r, id) {
		httpError(w, http.StatusConflict, "this is the only admin account; make someone else an admin first")
		return
	}
	rows, err := m.Store.Pool.Query(r.Context(), `SELECT id FROM workspaces WHERE user_id=$1`, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var wsIDs []uuid.UUID
	for rows.Next() {
		var wid uuid.UUID
		if rows.Scan(&wid) == nil {
			wsIDs = append(wsIDs, wid)
		}
	}
	rows.Close()
	// Best effort, like deleteWorkspace: a runtime that is down must not
	// make an account impossible to offboard. What could not be removed is
	// logged with its sandbox name so an operator can finish the job.
	if len(wsIDs) > 0 {
		rt, _, err := m.runtime(r.Context())
		for _, wid := range wsIDs {
			if err == nil {
				err = rt.Remove(r.Context(), sandboxName(wid))
			}
			if err != nil {
				slog.Warn("user delete: workspace not removed", "sandbox", sandboxName(wid), "err", err)
			}
		}
	}
	tag, err := m.Store.Pool.Exec(r.Context(), `DELETE FROM users WHERE id=$1`, id)
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

// deleteTeam refuses to silently revoke the team's keys: with keys present it
// answers 409 and the count, and goes ahead only with ?revoke_keys=true.
func (m *Server) deleteTeam(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var keys int
	m.Store.Pool.QueryRow(r.Context(), `SELECT count(*) FROM api_keys WHERE team_id=$1`, id).Scan(&keys)
	if keys > 0 && r.URL.Query().Get("revoke_keys") != "true" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "deleting this team revokes its API keys; confirm with ?revoke_keys=true",
			"keys":  keys,
		})
		return
	}
	tag, err := m.Store.Pool.Exec(r.Context(), `DELETE FROM teams WHERE id=$1`, id)
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
