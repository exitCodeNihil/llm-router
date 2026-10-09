package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// SignToken returns a stateless bearer token for subject that expires at exp:
// base64(subject|exp) "." base64(HMAC-SHA256). Each use derives its own key,
// so a token minted for one purpose never verifies for another.
func SignToken(key []byte, subject string, exp time.Time) string {
	payload := subject + "|" + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyToken returns the subject of a token signed with key that has not expired.
func VerifyToken(key []byte, value string) (string, error) {
	payloadB64, sigB64, ok := strings.Cut(value, ".")
	if !ok {
		return "", errors.New("malformed token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return "", errors.New("malformed token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return "", errors.New("malformed token")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", errors.New("bad signature")
	}
	i := strings.LastIndexByte(string(payload), '|')
	if i < 0 {
		return "", errors.New("malformed token")
	}
	exp, err := strconv.ParseInt(string(payload[i+1:]), 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", errors.New("expired token")
	}
	return string(payload[:i]), nil
}

// DeriveKey derives a signing key for one purpose from the deployment secret.
func DeriveKey(purpose, secret string) []byte {
	sum := sha256.Sum256([]byte(purpose + ":" + secret))
	return sum[:]
}
