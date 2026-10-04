package auth

import (
	"crypto/sha256"
	"testing"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

func TestResolveKeyRequiresLiveOwner(t *testing.T) {
	secret, hash, _ := GenerateKey()
	s := &snapshot.Snapshot{
		KeysByHash: map[[32]byte]*snapshot.Key{hash: {ID: "k1", UserID: "u1"}},
		UsersByID:  map[string]*snapshot.User{},
	}
	if ResolveKey(s, secret) != nil {
		t.Fatal("a key whose owner is disabled (absent from the snapshot) must not resolve")
	}
	s.UsersByID["u1"] = &snapshot.User{ID: "u1"}
	id := ResolveKey(s, secret)
	if id == nil || id.User == nil || id.User.ID != "u1" {
		t.Fatalf("live owner should resolve with the user attached, got %+v", id)
	}
	if sha256.Sum256([]byte(secret)) != hash {
		t.Fatal("hash mismatch")
	}
}
