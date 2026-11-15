package snapshot

import "testing"

func TestKeyringOpen(t *testing.T) {
	const cur, old, next = "current-key-0123456789", "old-key-0123456789", "next-key-0123456789"
	k := Keyring{Current: cur, Others: []string{old, next}}
	for want, key := range []string{cur, old, next} {
		enc, err := Encrypt(key, "secret")
		if err != nil {
			t.Fatal(err)
		}
		plain, which, err := k.Open(enc)
		if err != nil || plain != "secret" || which != want {
			t.Errorf("under %s: %q %d %v, want key %d", key, plain, which, err, want)
		}
	}
	// what Encrypt writes opens with the current key alone: a rollback to a
	// release without keyrings still reads it
	enc, _ := k.Encrypt("x")
	if p, err := Decrypt(cur, enc); err != nil || p != "x" {
		t.Errorf("keyring ciphertext under the current key: %q %v", p, err)
	}
	enc, _ = Encrypt("unrelated-key-0123456789", "secret")
	if _, which, err := k.Open(enc); err == nil || which != -1 {
		t.Errorf("a foreign key opened: %d %v", which, err)
	}
	if _, _, err := (Keyring{}).Open(enc); err == nil || err.Error() != "LLMR_ENCRYPTION_KEY is not set" {
		t.Errorf("no key: %v", err)
	}
}

func TestKeyFP(t *testing.T) {
	a, b := KeyFP("key-a-0123456789"), KeyFP("key-b-0123456789")
	if len(a) != 16 || a == b || a != KeyFP("key-a-0123456789") {
		t.Errorf("KeyFP: %q %q", a, b)
	}
}
