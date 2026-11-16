package console

import (
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// one is a keyring of a single key, as before rotation existed.
func one(key string) snapshot.Keyring { return snapshot.Keyring{Current: key} }

func TestSessionRoundTrip(t *testing.T) {
	key := "test-encryption-key"
	in := Session{UserID: "user-123", SID: "sid-1", Epoch: 4, Expires: time.Now().Add(time.Hour).Truncate(time.Second)}
	got, err := VerifySession(one(key), signSession(sessionKey(key), in))
	if err != nil || got != in {
		t.Fatalf("VerifySession = %+v, %v; want %+v", got, err, in)
	}
	if _, err := VerifySession(one("other-key"), signSession(sessionKey(key), in)); err == nil {
		t.Fatal("wrong key must fail verification")
	}
	expired := in
	expired.Expires = time.Now().Add(-time.Minute)
	if _, err := VerifySession(one(key), signSession(sessionKey(key), expired)); err == nil {
		t.Fatal("expired session must fail")
	}
	if _, err := VerifySession(one(key), "garbage"); err == nil {
		t.Fatal("garbage must fail")
	}
	// A cookie from before sessions had ids: the user id alone, epoch 0.
	old := auth.SignToken(sessionKey(key), "user-123", time.Now().Add(time.Hour))
	got, err = VerifySession(one(key), old)
	if err != nil || got.UserID != "user-123" || got.SID != "" || got.Epoch != 0 {
		t.Fatalf("old cookie = %+v, %v", got, err)
	}
	for _, subject := range []string{"u|sid", "u||3", "u|sid|x", "|sid|1"} {
		if _, err := VerifySession(one(key), auth.SignToken(sessionKey(key), subject, time.Now().Add(time.Hour))); err == nil {
			t.Errorf("subject %q must fail", subject)
		}
	}
}

// TestSessionRotation: through the three rotation stages a cookie signed by a
// replica on one stage verifies on a replica on the stage next to it, and a
// key once dropped no longer verifies.
func TestSessionRotation(t *testing.T) {
	const a, b = "rotation-key-a-0123456789", "rotation-key-b-0123456789"
	stages := []snapshot.Keyring{
		{Current: a},
		{Current: a, Others: []string{b}}, // b as NEXT
		{Current: b, Others: []string{a}}, // a as OLD
		{Current: b},
	}
	for i := 0; i+1 < len(stages); i++ {
		for _, pair := range [][2]snapshot.Keyring{{stages[i], stages[i+1]}, {stages[i+1], stages[i]}} {
			signer, verifier := pair[0], pair[1]
			in := Session{UserID: "u", SID: "s", Epoch: 1, Expires: time.Now().Add(time.Hour).Truncate(time.Second)}
			if got, err := VerifySession(verifier, signSession(sessionKey(signer.Current), in)); err != nil || got != in {
				t.Errorf("stage %d: signed by %+v, verified by %+v: %+v %v", i, signer, verifier, got, err)
			}
		}
	}
	in := Session{UserID: "u", SID: "s", Expires: time.Now().Add(time.Hour)}
	if _, err := VerifySession(stages[3], signSession(sessionKey(a), in)); err == nil {
		t.Error("a cookie under a removed key still verifies")
	}
}
