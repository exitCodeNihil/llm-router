package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
)

// Keyring is the encryption key in use and the ones still accepted during a
// rotation. Everything is written under Current; Others (LLMR_ENCRYPTION_KEYS_OLD,
// then LLMR_ENCRYPTION_KEY_NEXT) only read and verify, so replicas on
// neighbouring rotation stages understand each other.
type Keyring struct {
	Current string
	Others  []string
}

// All is Current followed by Others.
func (k Keyring) All() []string {
	return append([]string{k.Current}, k.Others...)
}

// Encrypt encrypts under Current.
func (k Keyring) Encrypt(plaintext string) ([]byte, error) {
	return Encrypt(k.Current, plaintext)
}

// Decrypt opens data with whichever key sealed it.
func (k Keyring) Decrypt(data []byte) (string, error) {
	plain, _, err := k.Open(data)
	return plain, err
}

// Open is Decrypt that also says which key opened data: 0 is Current, i is
// Others[i-1]. Trying each key in turn is safe: GCM's tag rejects a wrong
// one. On total failure the error is Current's, as with a single key.
func (k Keyring) Open(data []byte) (string, int, error) {
	plain, err := Decrypt(k.Current, data)
	if err == nil {
		return plain, 0, nil
	}
	for i, other := range k.Others {
		if p, oerr := Decrypt(other, data); oerr == nil {
			return p, i + 1, nil
		}
	}
	return "", -1, err
}

// KeyFP fingerprints a key so replicas can compare theirs without revealing
// it: its own label, so it is never the AES key or a signing key.
func KeyFP(passphrase string) string {
	sum := sha256.Sum256([]byte("llmr-key-fp:" + passphrase))
	return hex.EncodeToString(sum[:8])
}
