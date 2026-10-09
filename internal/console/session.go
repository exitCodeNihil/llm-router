// Package console serves the embedded SPA and handles login: OIDC SSO and
// the bootstrap admin token.
package console

import (
	"net/http"
	"time"

	"github.com/exitcodenihil/llm-router/internal/auth"
)

const sessionCookie = "llmr_session"
const sessionTTL = 12 * time.Hour

// ponytail: stateless HMAC sessions; add a denylist table if instant logout
// becomes a requirement.

func sessionKey(encryptionKey string) []byte { return auth.DeriveKey("llmr-session", encryptionKey) }

func signSession(key []byte, userID string, exp time.Time) string {
	return auth.SignToken(key, userID, exp)
}

// VerifySession returns the user id for a valid, unexpired session value.
func VerifySession(encryptionKey, value string) (string, error) {
	return auth.VerifyToken(sessionKey(encryptionKey), value)
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, encryptionKey, domain, userID string) {
	exp := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    signSession(sessionKey(encryptionKey), userID, exp),
		Path:     "/",
		Domain:   domain,
		Expires:  exp,
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})
}

// SessionUserID extracts the logged-in user id from the request cookie, or "".
func SessionUserID(encryptionKey string, r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	userID, err := VerifySession(encryptionKey, c.Value)
	if err != nil {
		return ""
	}
	return userID
}
