package auth

import (
	"testing"
	"time"
)

func TestVerifyTokenAny(t *testing.T) {
	keys := DeriveKeys("p", "current-0123456789", "old-0123456789")
	for i, k := range keys {
		if sub, err := VerifyTokenAny(keys, SignToken(k, "s", time.Now().Add(time.Minute))); err != nil || sub != "s" {
			t.Errorf("key %d: %q %v", i, sub, err)
		}
	}
	other := DeriveKey("p", "other-0123456789")
	if _, err := VerifyTokenAny(keys, SignToken(other, "s", time.Now().Add(time.Minute))); err == nil {
		t.Error("a token under another key verified")
	}
	if _, err := VerifyTokenAny(keys, SignToken(keys[1], "s", time.Now().Add(-time.Minute))); err == nil {
		t.Error("an expired token verified")
	}
	if _, err := VerifyTokenAny(nil, SignToken(keys[0], "s", time.Now().Add(time.Minute))); err == nil {
		t.Error("no keys verified a token")
	}
}
