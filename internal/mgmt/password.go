package mgmt

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/exitcodenihil/llm-router/internal/console"
)

// A password change signs the account out everywhere (session_epoch): a
// reset after a compromise must not leave the intruder's session alive.

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
	var epoch int
	err = m.Store.Pool.QueryRow(r.Context(), `UPDATE users SET password_hash=$2, session_epoch=session_epoch+1
		WHERE id=$1 RETURNING session_epoch`, id, hash).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if c := CallerFrom(r.Context()); c.UserID == id.String() {
		m.reissueSession(w, r, c.UserID, epoch)
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
	var epoch int
	if err := m.Store.Pool.QueryRow(r.Context(), `UPDATE users SET password_hash=$2, session_epoch=session_epoch+1
		WHERE id=$1 RETURNING session_epoch`, c.UserID, newHash).Scan(&epoch); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Every other session is gone; this one carries on under the new epoch.
	m.reissueSession(w, r, c.UserID, epoch)
	w.WriteHeader(http.StatusNoContent)
}

// reissueSession replaces the caller's session cookie with one at epoch, when
// they came in with one (the admin token has none).
func (m *Server) reissueSession(w http.ResponseWriter, r *http.Request, userID string, epoch int) {
	if s, ok := console.RequestSession(m.keys(), r); ok && s.UserID == userID {
		console.SetSessionCookie(w, r, m.EncryptionKey, m.CookieDomain, userID, epoch)
	}
}

// signOutUser ends every console session of a user (admin action).
func (m *Server) signOutUser(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return
	}
	tag, err := m.Store.Pool.Exec(r.Context(), `UPDATE users SET session_epoch=session_epoch+1 WHERE id=$1`, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	if c := CallerFrom(r.Context()); c.UserID == id.String() {
		console.ClearSessionCookie(w, m.CookieDomain)
	}
	w.WriteHeader(http.StatusNoContent)
}

// signOutSelf ends every console session of the caller, this one included.
func (m *Server) signOutSelf(w http.ResponseWriter, r *http.Request) {
	c := CallerFrom(r.Context())
	if c.UserID == "" {
		httpError(w, http.StatusBadRequest, "the admin token has no sessions; rotate LLMR_ADMIN_TOKEN instead")
		return
	}
	if _, err := m.Store.Pool.Exec(r.Context(),
		`UPDATE users SET session_epoch=session_epoch+1 WHERE id=$1`, c.UserID); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	console.ClearSessionCookie(w, m.CookieDomain)
	w.WriteHeader(http.StatusNoContent)
}
