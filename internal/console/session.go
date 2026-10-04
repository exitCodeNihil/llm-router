// Package console serves the embedded SPA and handles login: OIDC SSO and
// the bootstrap admin token.
package console

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const sessionCookie = "llmr_session"
const sessionTTL = 12 * time.Hour

// ponytail: stateless HMAC sessions; add a denylist table if instant logout
// becomes a requirement.

func sessionKey(encryptionKey string) []byte {
	sum := sha256.Sum256([]byte("llmr-session:" + encryptionKey))
	return sum[:]
}

func signSession(key []byte, userID string, exp time.Time) string {
	payload := userID + "|" + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifySession returns the user id for a valid, unexpired session value.
func VerifySession(encryptionKey, value string) (string, error) {
	key := sessionKey(encryptionKey)
	payloadB64, sigB64, ok := strings.Cut(value, ".")
	if !ok {
		return "", fmt.Errorf("malformed session")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return "", err
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", fmt.Errorf("bad signature")
	}
	userID, expStr, ok := strings.Cut(string(payload), "|")
	if !ok {
		return "", fmt.Errorf("malformed payload")
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", fmt.Errorf("expired session")
	}
	return userID, nil
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
