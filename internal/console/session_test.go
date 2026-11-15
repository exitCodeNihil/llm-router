package console

import (
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/auth"
)

func TestSessionRoundTrip(t *testing.T) {
	key := "test-encryption-key"
	in := Session{UserID: "user-123", SID: "sid-1", Epoch: 4, Expires: time.Now().Add(time.Hour).Truncate(time.Second)}
	got, err := VerifySession(key, signSession(sessionKey(key), in))
	if err != nil || got != in {
		t.Fatalf("VerifySession = %+v, %v; want %+v", got, err, in)
	}
	if _, err := VerifySession("other-key", signSession(sessionKey(key), in)); err == nil {
		t.Fatal("wrong key must fail verification")
	}
	expired := in
	expired.Expires = time.Now().Add(-time.Minute)
	if _, err := VerifySession(key, signSession(sessionKey(key), expired)); err == nil {
		t.Fatal("expired session must fail")
	}
	if _, err := VerifySession(key, "garbage"); err == nil {
		t.Fatal("garbage must fail")
	}
	// A cookie from before sessions had ids: the user id alone, epoch 0.
	old := auth.SignToken(sessionKey(key), "user-123", time.Now().Add(time.Hour))
	got, err = VerifySession(key, old)
	if err != nil || got.UserID != "user-123" || got.SID != "" || got.Epoch != 0 {
		t.Fatalf("old cookie = %+v, %v", got, err)
	}
	for _, subject := range []string{"u|sid", "u||3", "u|sid|x", "|sid|1"} {
		if _, err := VerifySession(key, auth.SignToken(sessionKey(key), subject, time.Now().Add(time.Hour))); err == nil {
			t.Errorf("subject %q must fail", subject)
		}
	}
}
