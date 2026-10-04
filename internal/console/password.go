package console

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Password auth: first-run setup creates the initial admin; after that,
// email+password login works alongside SSO and the bootstrap token.
// ponytail: no login rate limiting beyond bcrypt's inherent cost; add a
// per-IP limiter if this ever faces the open internet directly.

func (a *Auth) registerPassword(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/setup", a.handleSetupStatus)
	mux.HandleFunc("POST /auth/setup", a.handleSetup)
	mux.HandleFunc("POST /auth/password", a.handlePasswordLogin)
}

// setupNeeded is true only while there are no users at all. It must not key
// on "an enabled admin exists": disabling the last admin would otherwise
// reopen an unauthenticated route to a fresh admin account.
func (a *Auth) setupNeeded(r *http.Request) (bool, error) {
	var exists bool
	err := a.Store.Pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM users)`).Scan(&exists)
	return !exists, err
}

func (a *Auth) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	needed, err := a.setupNeeded(r)
	if err != nil {
		http.Error(w, "setup status unavailable", http.StatusInternalServerError)
		return
	}
	_, ssoErr := a.loadSSO(r.Context())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"needed": needed, "sso": ssoErr == nil})
}

func (a *Auth) handleSetup(w http.ResponseWriter, r *http.Request) {
	needed, err := a.setupNeeded(r)
	if err != nil || !needed {
		http.Error(w, "setup is already complete", http.StatusForbidden)
		return
	}
	var req struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil ||
		req.Email == "" || len(req.Password) < 8 {
		http.Error(w, "email and a password of at least 8 characters are required", http.StatusBadRequest)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "hashing failed", http.StatusInternalServerError)
		return
	}
	id := uuid.New()
	// One statement, so two concurrent setup calls cannot both win.
	tag, err := a.Store.Pool.Exec(r.Context(),
		`INSERT INTO users (id, email, name, role, password_hash)
		 SELECT $1,$2,$3,'admin',$4 WHERE NOT EXISTS (SELECT 1 FROM users)`,
		id, req.Email, req.Name, hash)
	if err != nil {
		http.Error(w, "could not create the admin account", http.StatusBadRequest)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "setup is already complete", http.StatusForbidden)
		return
	}
	setSessionCookie(w, r, a.EncryptionKey, a.CookieDomain, id.String())
	w.WriteHeader(http.StatusNoContent)
}

func (a *Auth) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	var userID string
	var hash []byte
	err := a.Store.Pool.QueryRow(r.Context(),
		`SELECT id, password_hash FROM users WHERE email=$1 AND NOT disabled`, req.Email).
		Scan(&userID, &hash)
	if err != nil || len(hash) == 0 ||
		bcrypt.CompareHashAndPassword(hash, []byte(req.Password)) != nil {
		http.Error(w, "incorrect email or password", http.StatusUnauthorized)
		return
	}
	setSessionCookie(w, r, a.EncryptionKey, a.CookieDomain, userID)
	w.WriteHeader(http.StatusNoContent)
}

// HashPassword is used by the management API's set-password endpoints.
func HashPassword(password string) ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
}

// CheckPassword verifies a password against a stored hash.
func CheckPassword(hash []byte, password string) bool {
	return len(hash) > 0 && bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}
