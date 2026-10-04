package mgmt

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/console"
)

// setUserPassword lets an admin set/reset any user's password.
func (m *Server) setUserPassword(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	req, ok := decode[struct {
		Password string `json:"password"`
	}](w, r)
	if !ok {
		return
	}
	if len(req.Password) < 8 {
		httpError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	hash, err := console.HashPassword(req.Password)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "hashing failed")
		return
	}
	tag, err := m.Store.Pool.Exec(r.Context(), `UPDATE users SET password_hash=$2 WHERE id=$1`, id, hash)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// changeOwnPassword verifies the current password before setting a new one.
func (m *Server) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	if c.UserID == "" {
		httpError(w, http.StatusBadRequest, "token sessions have no password; sign in as a user")
		return
	}
	req, ok := decode[struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}](w, r)
	if !ok {
		return
	}
	if len(req.New) < 8 {
		httpError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	var hash []byte
	if err := m.Store.Pool.QueryRow(r.Context(),
		`SELECT password_hash FROM users WHERE id=$1`, c.UserID).Scan(&hash); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// users provisioned by SSO have no password yet; allow setting one directly
	if len(hash) > 0 && !console.CheckPassword(hash, req.Current) {
		httpError(w, http.StatusForbidden, "current password is incorrect")
		return
	}
	newHash, err := console.HashPassword(req.New)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "hashing failed")
		return
	}
	if _, err := m.Store.Pool.Exec(r.Context(),
		`UPDATE users SET password_hash=$2 WHERE id=$1`, c.UserID, newHash); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
