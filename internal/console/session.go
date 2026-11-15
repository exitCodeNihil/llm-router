// Package console serves the embedded SPA and handles login: OIDC SSO and
// the bootstrap admin token.
package console

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exitcodenihil/llm-router/internal/auth"
)

const sessionCookie = "llmr_session"
const sessionTTL = 12 * time.Hour

// Sessions are stateless HMAC cookies carrying a random id and the user's
// session epoch at issue. Whoever authenticates a session must also check it
// against the database (SessionLiveSQL): an epoch below users.session_epoch
// or an id in revoked_sessions is signed out.

// Session is a verified session cookie.
type Session struct {
	UserID string
	// SID is "" and Epoch 0 for a cookie from before revocation existed;
	// such a cookie dies with the user's first epoch bump.
	SID     string
	Epoch   int
	Expires time.Time
}

// SessionLiveSQL is the condition, on a users row aliased u, that a session
// is still signed in: $epoch and $sid are the session's own. One subselect,
// so it rides on the user lookup every request already makes.
func SessionLiveSQL(epoch, sid string) string {
	return `u.session_epoch <= ` + epoch + ` AND NOT EXISTS (SELECT 1 FROM revoked_sessions rs
		WHERE rs.sid = ` + sid + ` AND rs.expires_at > now())`
}

func sessionKey(encryptionKey string) []byte { return auth.DeriveKey("llmr-session", encryptionKey) }

func newSID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func signSession(key []byte, s Session) string {
	return auth.SignToken(key, s.UserID+"|"+s.SID+"|"+strconv.Itoa(s.Epoch), s.Expires)
}

// VerifySession returns the session in a valid, unexpired cookie value. It
// checks the signature only; revocation is the database's (SessionLiveSQL).
func VerifySession(encryptionKey, value string) (Session, error) {
	subject, exp, err := auth.VerifyTokenExpiry(sessionKey(encryptionKey), value)
	if err != nil {
		return Session{}, err
	}
	s := Session{Expires: exp}
	parts := strings.Split(subject, "|")
	switch len(parts) {
	case 1: // user id only: issued before sessions had ids
		s.UserID = parts[0]
	case 3:
		s.UserID, s.SID = parts[0], parts[1]
		if s.Epoch, err = strconv.Atoi(parts[2]); err != nil || s.SID == "" {
			return Session{}, errors.New("malformed session")
		}
	default:
		return Session{}, errors.New("malformed session")
	}
	if s.UserID == "" {
		return Session{}, errors.New("malformed session")
	}
	return s, nil
}

// SetSessionCookie signs the user in on this response with a fresh session
// at epoch, the user's current users.session_epoch.
func SetSessionCookie(w http.ResponseWriter, r *http.Request, encryptionKey, domain, userID string, epoch int) {
	s := Session{UserID: userID, SID: newSID(), Epoch: epoch, Expires: time.Now().Add(sessionTTL)}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    signSession(sessionKey(encryptionKey), s),
		Path:     "/",
		Domain:   domain,
		Expires:  s.Expires,
		HttpOnly: true,
		Secure:   secureRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie removes the session cookie from the browser.
func ClearSessionCookie(w http.ResponseWriter, domain string) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, Domain: domain})
}

// secureRequest: whether cookies set on this response should be Secure.
func secureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// RequestSession returns the request's session cookie when its signature is
// valid and it has not expired. The caller still checks revocation.
func RequestSession(encryptionKey string, r *http.Request) (Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Session{}, false
	}
	s, err := VerifySession(encryptionKey, c.Value)
	return s, err == nil
}

// HasSession reports whether the request carries a session cookie, valid or not.
func HasSession(r *http.Request) bool {
	_, err := r.Cookie(sessionCookie)
	return err == nil
}

// PruneSessions forgets signed-out sessions whose cookies have expired: they
// fail verification on their own now.
func PruneSessions(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `DELETE FROM revoked_sessions WHERE expires_at <= now()`)
	return err
}
