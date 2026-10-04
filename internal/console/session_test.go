package console

import (
	"testing"
	"time"
)

func TestSessionRoundTrip(t *testing.T) {
	key := "test-encryption-key"
	val := signSession(sessionKey(key), "user-123", time.Now().Add(time.Hour))
	got, err := VerifySession(key, val)
	if err != nil || got != "user-123" {
		t.Fatalf("VerifySession = %q, %v", got, err)
	}
	if _, err := VerifySession("other-key", val); err == nil {
		t.Fatal("wrong key must fail verification")
	}
	expired := signSession(sessionKey(key), "user-123", time.Now().Add(-time.Minute))
	if _, err := VerifySession(key, expired); err == nil {
		t.Fatal("expired session must fail")
	}
	if _, err := VerifySession(key, "garbage"); err == nil {
		t.Fatal("garbage must fail")
	}
}
