package gwsync

import (
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// TestRotatedKeyEntries: a key in a rotation's grace period reaches gateways
// as two entries with its id, the old one expiring with the grace period, in
// an order that does not depend on map iteration.
func TestRotatedKeyEntries(t *testing.T) {
	s := fixture()
	cur := s.KeysByHash[[32]byte{2}]
	grace := time.Unix(1900000000, 0) // before the key's own expiry
	old := *cur
	old.OldHash, old.ExpiresAt = true, &grace
	// A hash that sorts before the current one, so map order would show.
	s.KeysByHash[[32]byte{0, 9}] = &old

	g := &snapshot.GatewayGroup{ID: "g", Name: "all"}
	first, _ := json.Marshal(buildConfig(s, g, schemaVersion).cfg)
	for range 20 {
		if again, _ := json.Marshal(buildConfig(s, g, schemaVersion).cfg); string(again) != string(first) {
			t.Fatal("config order changes between builds")
		}
	}
	b := buildConfig(s, g, schemaVersion)
	var entries []Key
	for _, k := range b.cfg.Keys {
		if k.ID == "k-alice-own" {
			entries = append(entries, k)
		}
	}
	if len(entries) != 2 {
		t.Fatalf("rotated key has %d entries, want 2", len(entries))
	}
	oldHex, curHex := hex.EncodeToString(make32(0, 9)), hex.EncodeToString(make32(2))
	if entries[0].Hash != oldHex || entries[1].Hash != curHex {
		t.Errorf("entries not sorted by hash: %s, %s", entries[0].Hash, entries[1].Hash)
	}
	if entries[0].ExpiresAt != grace.Unix() || entries[1].ExpiresAt != cur.ExpiresAt.Unix() {
		t.Errorf("expiries = %d, %d", entries[0].ExpiresAt, entries[1].ExpiresAt)
	}
	// What the sync handler checks uploads against is the key itself.
	if k := b.keys["k-alice-own"]; k == nil || k.OldHash {
		t.Errorf("keys by id holds %+v", k)
	}
}

func make32(b ...byte) []byte {
	out := make([]byte, 32)
	copy(out, b)
	return out
}
