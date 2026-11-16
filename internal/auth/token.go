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
	subject, _, err := VerifyTokenExpiry(key, value)
	return subject, err
}

// VerifyTokenExpiry is VerifyToken that also returns when the token expires.
func VerifyTokenExpiry(key []byte, value string) (string, time.Time, error) {
	return VerifyTokenExpiryAny([][]byte{key}, value)
}

// VerifyTokenAny is VerifyToken accepting a token signed with any of keys:
// during an encryption-key rotation a token signed before it, or by a replica
// on the next stage, stays valid.
func VerifyTokenAny(keys [][]byte, value string) (string, error) {
	subject, _, err := VerifyTokenExpiryAny(keys, value)
	return subject, err
}

// VerifyTokenExpiryAny is VerifyTokenAny that also returns when the token expires.
func VerifyTokenExpiryAny(keys [][]byte, value string) (string, time.Time, error) {
	payloadB64, sigB64, ok := strings.Cut(value, ".")
	if !ok {
		return "", time.Time{}, errors.New("malformed token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return "", time.Time{}, errors.New("malformed token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return "", time.Time{}, errors.New("malformed token")
	}
	signed := false
	for _, key := range keys {
		mac := hmac.New(sha256.New, key)
		mac.Write(payload)
		if hmac.Equal(sig, mac.Sum(nil)) {
			signed = true
			break
		}
	}
	if !signed {
		return "", time.Time{}, errors.New("bad signature")
	}
	i := strings.LastIndexByte(string(payload), '|')
	if i < 0 {
		return "", time.Time{}, errors.New("malformed token")
	}
	exp, err := strconv.ParseInt(string(payload[i+1:]), 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", time.Time{}, errors.New("expired token")
	}
	return string(payload[:i]), time.Unix(exp, 0), nil
}

// DeriveKey derives a signing key for one purpose from the deployment secret.
func DeriveKey(purpose, secret string) []byte {
	sum := sha256.Sum256([]byte(purpose + ":" + secret))
	return sum[:]
}

// DeriveKeys is DeriveKey for each of secrets, in order: the first signs,
// all of them verify (VerifyTokenAny).
func DeriveKeys(purpose string, secrets ...string) [][]byte {
	keys := make([][]byte, len(secrets))
	for i, s := range secrets {
		keys[i] = DeriveKey(purpose, s)
	}
	return keys
}
