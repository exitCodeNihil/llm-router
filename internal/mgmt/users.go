package mgmt

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/exitcodenihil/llm-router/internal/audit"
	"github.com/exitcodenihil/llm-router/internal/workspace"
)

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// lastAdmin reports whether id is the only enabled admin. Removing that
// account (delete, disable, demote) locks everyone out of administration,
// with the first-run setup screen as the only way back in.
func lastAdmin(ctx context.Context, q queryRower, id uuid.UUID) bool {
	var n int
	var isAdmin bool
	q.QueryRow(ctx,
		`SELECT count(*), bool_or(id=$1) FROM users WHERE role='admin' AND NOT disabled`, id).Scan(&n, &isAdmin)
	return isAdmin && n == 1
}

const onlyAdmin = "this is the only admin account; make someone else an admin first"

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
	if (demote || disable) && lastAdmin(r.Context(), m.Store.Pool, id) {
		httpError(w, http.StatusConflict, onlyAdmin)
		return
	}
	if !m.applyPatch(w, r, "users", id, userPatchCols, fields) {
		return
	}
	m.bump(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// deletedBy is the acting user for deleted_entities; nil for the admin token.
func deletedBy(c *Caller) *string {
	if c == nil || c.UserID == "" {
		return nil
	}
	return &c.UserID
}

var errLastAdmin = errors.New(onlyAdmin)

// removeUser deletes a live user in tx, keeping their email and their keys'
// names (the cascade removes the keys). It returns the user's workspaces,
// whose sandboxes the caller removes once tx commits, and false when there
// is no such user.
func removeUser(ctx context.Context, tx pgx.Tx, id uuid.UUID, by *string) ([]uuid.UUID, bool, error) {
	if lastAdmin(ctx, tx, id) {
		return nil, true, errLastAdmin
	}
	rows, err := tx.Query(ctx, `SELECT id FROM workspaces WHERE user_id=$1`, id)
	if err != nil {
		return nil, false, err
	}
	wsIDs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO deleted_entities (kind, id, label, user_id, team_id, deleted_by)
		SELECT 'key', id, name, user_id, team_id, $2 FROM api_keys WHERE user_id=$1
		ON CONFLICT (kind, id) DO NOTHING`, id, by); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO deleted_entities (kind, id, label, user_id, deleted_by)
		SELECT 'user', id, email, id, $2 FROM users WHERE id=$1
		ON CONFLICT (kind, id) DO NOTHING`, id, by); err != nil {
		return nil, false, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id=$1`, id)
	if err != nil {
		return nil, false, err
	}
	return wsIDs, tag.RowsAffected() > 0, nil
}

// removeSandboxes removes the containers and volumes behind workspaces whose
// rows are gone. Best effort, like deleteWorkspace: a runtime that is down
// must not make an account impossible to offboard. It returns the sandboxes
// it could not remove, also logged so an operator can finish the job.
func (m *Server) removeSandboxes(ctx context.Context, wsIDs []uuid.UUID) []string {
	if len(wsIDs) == 0 {
		return nil
	}
	var left []string
	rt, _, err := m.runtime(ctx)
	for _, wid := range wsIDs {
		if err == nil {
			err = rt.Remove(ctx, sandboxName(wid))
		}
		if err != nil {
			left = append(left, sandboxName(wid))
			slog.Warn("user delete: workspace not removed", "sandbox", sandboxName(wid), "err", err)
		}
	}
	return left
}

// deleteUser removes the account and everything the database cascade cannot
// reach: the containers and volumes behind the user's workspaces.
func (m *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var wsIDs []uuid.UUID
	var found bool
	err = pgx.BeginFunc(r.Context(), m.Store.Pool, func(tx pgx.Tx) error {
		var err error
		wsIDs, found, err = removeUser(r.Context(), tx, id, deletedBy(CallerFrom(r.Context())))
		return err
	})
	switch {
	case errors.Is(err, errLastAdmin):
		httpError(w, http.StatusConflict, onlyAdmin)
		return
	case err != nil:
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	case !found:
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	m.removeSandboxes(r.Context(), wsIDs)
	m.bump(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// eraseUser removes what identifies a person: their account, if it is still
// there, and their email and key names wherever llm-router kept them since.
// Usage rows keep the user's id, which then maps to no one.
func (m *Server) eraseUser(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	ctx := r.Context()
	var wsIDs []uuid.UUID
	err = pgx.BeginFunc(ctx, m.Store.Pool, func(tx pgx.Tx) error {
		var email string
		err := tx.QueryRow(ctx, `SELECT email FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&email)
		switch {
		case err == nil:
			if wsIDs, _, err = removeUser(ctx, tx, id, deletedBy(CallerFrom(ctx))); err != nil {
				return err
			}
		case errors.Is(err, pgx.ErrNoRows):
			var erased bool
			if err := tx.QueryRow(ctx, `SELECT label, erased_at IS NOT NULL FROM deleted_entities
				WHERE kind='user' AND id=$1 FOR UPDATE`, id).Scan(&email, &erased); err != nil {
				return err
			}
			if erased {
				email = ""
			}
		default:
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE deleted_entities SET label='erased', erased_at=coalesce(erased_at, now())
			WHERE kind='user' AND id=$1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE deleted_entities SET label='erased key', erased_at=coalesce(erased_at, now())
			WHERE kind='key' AND user_id=$1`, id); err != nil {
			return err
		}
		if err := audit.MaintainTx(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE audit_events SET actor_email=NULL
			WHERE actor_user_id=$1 AND actor_email IS NOT NULL`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE audit_events SET detail=NULL WHERE detail IS NOT NULL AND (
				(target_type='user' AND target_id=$1)
				OR (target_type='team_member' AND target_id LIKE '%/' || $1)
				OR (target_type='key' AND target_id IN (
					SELECT id::text FROM deleted_entities WHERE kind='key' AND user_id=$1::uuid))
				OR ($2 <> '' AND detail::text ILIKE '%' || $2 || '%' ESCAPE '\'))`, id.String(), likeEscape(email)); err != nil {
			return err
		}
		return dropFromWorkspaceAllow(ctx, tx, id.String())
	})
	switch {
	case errors.Is(err, errLastAdmin):
		httpError(w, http.StatusConflict, onlyAdmin)
		return
	case errors.Is(err, pgx.ErrNoRows):
		httpError(w, http.StatusNotFound, "not found")
		return
	case err != nil:
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Gateways hold a hash of each user's email: the next config drops it.
	m.bump(ctx)
	notReached := []string{"application logs", "database backups", "traces already exported to telemetry destinations"}
	for _, s := range m.removeSandboxes(ctx, wsIDs) {
		notReached = append(notReached, "workspace volume "+s)
	}
	writeJSON(w, http.StatusOK, map[string]any{"erased": true, "not_reached": notReached})
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func likeEscape(s string) string { return likeEscaper.Replace(s) }

// dropFromWorkspaceAllow takes a user out of the workspaces allow-list.
func dropFromWorkspaceAllow(ctx context.Context, tx pgx.Tx, userID string) error {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT value FROM settings WHERE key='workspaces' FOR UPDATE`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg workspace.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	keep := cfg.Allow[:0]
	for _, s := range cfg.Allow {
		if s.ScopeType != "user" || s.ScopeValue != userID {
			keep = append(keep, s)
		}
	}
	if len(keep) == len(cfg.Allow) {
		return nil
	}
	cfg.Allow = keep
	val, _ := json.Marshal(cfg)
	_, err = tx.Exec(ctx, `UPDATE settings SET value=$1 WHERE key='workspaces'`, val)
	return err
}

func (m *Server) listDeletedUsers(w http.ResponseWriter, r *http.Request) {
	m.list(w, r, `SELECT id, label, deleted_at, erased_at FROM deleted_entities
		WHERE kind='user' ORDER BY deleted_at DESC LIMIT 200`)
}

// deleteTeam refuses to silently revoke the team's keys: with keys present it
// answers 409 and the count, and goes ahead only with ?revoke_keys=true.
func (m *Server) deleteTeam(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	ctx, by := r.Context(), deletedBy(CallerFrom(r.Context()))
	var keys int
	var found bool
	err = pgx.BeginFunc(ctx, m.Store.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE team_id=$1`, id).Scan(&keys); err != nil {
			return err
		}
		if keys > 0 && r.URL.Query().Get("revoke_keys") != "true" {
			return nil
		}
		keys = 0
		if _, err := tx.Exec(ctx, `INSERT INTO deleted_entities (kind, id, label, user_id, team_id, deleted_by)
			SELECT 'key', id, name, user_id, team_id, $2 FROM api_keys WHERE team_id=$1
			ON CONFLICT (kind, id) DO NOTHING`, id, by); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO deleted_entities (kind, id, label, deleted_by)
			SELECT 'team', id, name, $2 FROM teams WHERE id=$1
			ON CONFLICT (kind, id) DO NOTHING`, id, by); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM teams WHERE id=$1`, id)
		found = tag.RowsAffected() > 0
		return err
	})
	switch {
	case err != nil:
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	case keys > 0:
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "deleting this team revokes its API keys; confirm with ?revoke_keys=true",
			"keys":  keys,
		})
		return
	case !found:
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	m.removeFromScopes(ctx, "team_values", id.String())
	m.bump(ctx)
	w.WriteHeader(http.StatusNoContent)
}

func (m *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, _, ok := m.keyOwnership(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, m.Store.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO deleted_entities (kind, id, label, user_id, team_id, deleted_by)
			SELECT 'key', id, name, user_id, team_id, $2 FROM api_keys WHERE id=$1
			ON CONFLICT (kind, id) DO NOTHING`, id, deletedBy(CallerFrom(ctx))); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM api_keys WHERE id=$1`, id)
		return err
	})
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	m.bump(ctx)
	w.WriteHeader(http.StatusNoContent)
}
