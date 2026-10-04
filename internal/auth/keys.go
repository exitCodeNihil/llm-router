// Package auth resolves inbound credentials (gateway API keys, and later
// Entra/GCP identity tokens) to an Identity.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"math/big"
	"strings"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

const KeyPrefix = "llmr_"

// Identity is the resolved caller placed on the request context.
type Identity struct {
	Key  *snapshot.Key // nil for cloud-token callers
	User *snapshot.User
	Team *snapshot.Team
}

// ModelAllowed applies the model policy: a model must pass every restriction
// the caller carries — the team's, the user's and the key's. A nil list at
// any level means that level does not restrict. So an admin can pin a team
// or a person to a set of models once, and no key they mint can widen it.
func (id *Identity) ModelAllowed(model string) bool {
	if id == nil {
		return false
	}
	if id.Team != nil && id.Team.AllowedModels != nil && !id.Team.AllowedModels[model] {
		return false
	}
	if id.User != nil {
		// A person's own policy wins; otherwise their teams decide, as the
		// union of what those teams allow.
		policy := id.User.AllowedModels
		if policy == nil {
			policy = id.User.TeamModels
		}
		if policy != nil && !policy[model] {
			return false
		}
	}
	if id.Key != nil && id.Key.AllowedModels != nil && !id.Key.AllowedModels[model] {
		return false
	}
	return true
}

// GenerateKey returns (secret, sha256 hash, display prefix).
func GenerateKey() (secret string, hash [32]byte, display string) {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, 43) // 43 base62 chars ≈ 256 bits
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[n.Int64()]
	}
	secret = KeyPrefix + string(b)
	return secret, sha256.Sum256([]byte(secret)), secret[:13]
}

// ResolveKey looks up a bearer secret in the snapshot. Returns nil if unknown,
// disabled (not in snapshot), or expired.
func ResolveKey(s *snapshot.Snapshot, secret string) *Identity {
	if !strings.HasPrefix(secret, KeyPrefix) {
		return nil
	}
	h := sha256.Sum256([]byte(secret))
	k := s.KeysByHash[h]
	if k == nil {
		return nil
	}
	if k.ExpiresAt != nil && time.Now().After(*k.ExpiresAt) {
		return nil
	}
	id := &Identity{Key: k}
	if k.UserID != "" {
		// Disabled users are not in the snapshot. Their keys must die with
		// them rather than keep working with no user-level limits.
		id.User = s.UsersByID[k.UserID]
		if id.User == nil {
			return nil
		}
	}
	if k.TeamID != "" {
		id.Team = s.TeamsByID[k.TeamID]
	}
	return id
}
