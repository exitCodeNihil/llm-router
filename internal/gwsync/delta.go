package gwsync

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"sync"
)

// Schema 3 lets a gateway that holds a config take only what changed since.
//
// Every key, user and team carries a hash of its content (H), computed here;
// a gateway never computes one. A config's digest is a sum over its entries,
// so both sides can check they hold the same config without serialising it
// the same way. A gateway sends the version and digest it holds; when they
// are ones this control plane knows, it answers with a Delta.

// Delta is what changed between the config a gateway holds and the current
// one. A section that is absent did not change. Keys are named by their
// secret's hash: a rotated key is two entries with one id.
type Delta struct {
	Schema      int    `json:"schema"`
	Delta       bool   `json:"delta"` // always true: tells it from a Config
	Epoch       string `json:"epoch"`
	BaseVersion int64  `json:"base_version"`
	BaseDigest  string `json:"base_digest"`
	Version     int64  `json:"version"`
	Digest      string `json:"digest"`

	Keys   *Changes[Key]  `json:"keys,omitempty"`
	Users  *Changes[User] `json:"users,omitempty"`
	Teams  *Changes[Team] `json:"teams,omitempty"`
	Emails *EmailChanges  `json:"emails,omitempty"`
	Rest   *Rest          `json:"rest,omitempty"`
}

type Changes[T any] struct {
	Upsert []T      `json:"upsert,omitempty"`
	Remove []string `json:"remove,omitempty"`
}

type EmailChanges struct {
	Upsert map[string]string `json:"upsert,omitempty"`
	Remove []string          `json:"remove,omitempty"`
}

// Rest is everything in a config that is not a key, user, team or email. It
// is small and replaced whole.
type Rest struct {
	Group       Group            `json:"group"`
	Providers   []Provider       `json:"providers"`
	Deployments []Deployment     `json:"deployments"`
	Prices      map[string]Price `json:"prices"`
	Issuers     []Issuer         `json:"issuers,omitempty"`
	Telemetry   *Telemetry       `json:"telemetry,omitempty"`
	RestHash    string           `json:"rest_hash"`
}

// entryHash is an entry's content hash: of its JSON without H.
func entryHash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// digest sums entries. Each is SHA-256(section, 0, key, 0, hash) read as four
// little-endian 64-bit lanes; the digest is the lanes' wrapping sums. The
// order entries are added in does not matter.
type digest [4]uint64

func (d *digest) add(section, key, hash string) {
	h := sha256.New()
	h.Write([]byte(section))
	h.Write([]byte{0})
	h.Write([]byte(key))
	h.Write([]byte{0})
	h.Write([]byte(hash))
	var sum [32]byte
	h.Sum(sum[:0])
	for i := range d {
		d[i] += binary.LittleEndian.Uint64(sum[i*8:])
	}
}

func (d digest) String() string {
	var b [32]byte
	for i, lane := range d {
		binary.LittleEndian.PutUint64(b[i*8:], lane)
	}
	return hex.EncodeToString(b[:])
}

// seal fills in a schema 3 config's entry hashes, rest hash and digest.
func seal(c *Config, epoch string) {
	c.Epoch = epoch
	var d digest
	for i := range c.Keys {
		c.Keys[i].H = ""
		c.Keys[i].H = entryHash(c.Keys[i])
		d.add("keys", c.Keys[i].Hash, c.Keys[i].H)
	}
	for i := range c.Users {
		c.Users[i].H = ""
		c.Users[i].H = entryHash(c.Users[i])
		d.add("users", c.Users[i].ID, c.Users[i].H)
	}
	for i := range c.Teams {
		c.Teams[i].H = ""
		c.Teams[i].H = entryHash(c.Teams[i])
		d.add("teams", c.Teams[i].ID, c.Teams[i].H)
	}
	for hash, user := range c.Emails {
		d.add("emails", hash, user)
	}
	r := rest(c)
	r.RestHash = ""
	c.RestHash = entryHash(r)
	d.add("rest", "", c.RestHash)
	c.Digest = d.String()
}

func rest(c *Config) Rest {
	return Rest{Group: c.Group, Providers: c.Providers, Deployments: c.Deployments, Prices: c.Prices,
		Issuers: c.Issuers, Telemetry: c.Telemetry, RestHash: c.RestHash}
}

// tracker remembers, for one group and schema, at which version each entry
// of the current config last changed and which entries were removed since
// floor, so a delta from any version it has seen can be cut without keeping
// the older configs.
type tracker struct {
	mu      sync.Mutex
	version int64
	floor   int64 // deltas are possible from this version on
	entries map[entryKey]entryState
	removed map[entryKey]int64 // version it went
	restAt  int64
	rest    string
	digests map[int64]string // of the versions built here
}

type entryKey struct{ section, key string }

type entryState struct {
	hash string
	at   int64 // version it was added or last changed
}

// Past this many remembered removals the tracker starts over: gateways on
// older versions then take one full config.
const maxRemoved = 50000

func (t *tracker) forget(version int64) {
	t.floor, t.entries, t.removed = version, map[entryKey]entryState{}, map[entryKey]int64{}
	t.digests = map[int64]string{}
	t.rest, t.restAt = "", version
}

// advance moves the tracker to c, a newer version of the config.
func (t *tracker) advance(c *Config) {
	if t.entries == nil {
		t.forget(c.Version)
	}
	seen := make(map[entryKey]bool, len(t.entries))
	put := func(section, key, hash string) {
		k := entryKey{section, key}
		seen[k] = true
		if old, ok := t.entries[k]; !ok || old.hash != hash {
			t.entries[k] = entryState{hash, c.Version}
		}
		delete(t.removed, k)
	}
	for _, k := range c.Keys {
		put("keys", k.Hash, k.H)
	}
	for _, u := range c.Users {
		put("users", u.ID, u.H)
	}
	for _, tm := range c.Teams {
		put("teams", tm.ID, tm.H)
	}
	for hash, user := range c.Emails {
		put("emails", hash, user)
	}
	for k := range t.entries {
		if !seen[k] {
			delete(t.entries, k)
			t.removed[k] = c.Version
		}
	}
	if t.rest != c.RestHash {
		t.rest, t.restAt = c.RestHash, c.Version
	}
	if len(t.removed) > maxRemoved {
		clear(t.removed)
		clear(t.digests)
		t.floor = c.Version
	}
	if len(t.digests) > 256 {
		for v := range t.digests {
			if v < c.Version-128 {
				delete(t.digests, v)
			}
		}
	}
	t.version = c.Version
	t.digests[c.Version] = c.Digest
}

// since cuts the delta from base to c, or nil when base is not a version
// this tracker can answer for: older than it remembers, never built here, or
// with another digest (another control plane built it differently).
func (t *tracker) since(c *Config, base int64, baseDigest string) *Delta {
	if t.version != c.Version || base < t.floor || base >= c.Version || baseDigest == "" || t.digests[base] != baseDigest {
		return nil
	}
	d := &Delta{Schema: c.Schema, Delta: true, Epoch: c.Epoch, BaseVersion: base, BaseDigest: baseDigest,
		Version: c.Version, Digest: c.Digest}
	changed := func(section, key string) bool { return t.entries[entryKey{section, key}].at > base }
	keys, users, teams, emails := &Changes[Key]{}, &Changes[User]{}, &Changes[Team]{}, &EmailChanges{Upsert: map[string]string{}}
	for _, k := range c.Keys {
		if changed("keys", k.Hash) {
			keys.Upsert = append(keys.Upsert, k)
		}
	}
	for _, u := range c.Users {
		if changed("users", u.ID) {
			users.Upsert = append(users.Upsert, u)
		}
	}
	for _, tm := range c.Teams {
		if changed("teams", tm.ID) {
			teams.Upsert = append(teams.Upsert, tm)
		}
	}
	for hash, user := range c.Emails {
		if changed("emails", hash) {
			emails.Upsert[hash] = user
		}
	}
	for k, at := range t.removed {
		if at <= base {
			continue
		}
		switch k.section {
		case "keys":
			keys.Remove = append(keys.Remove, k.key)
		case "users":
			users.Remove = append(users.Remove, k.key)
		case "teams":
			teams.Remove = append(teams.Remove, k.key)
		case "emails":
			emails.Remove = append(emails.Remove, k.key)
		}
	}
	if len(keys.Upsert)+len(keys.Remove) > 0 {
		d.Keys = keys
	}
	if len(users.Upsert)+len(users.Remove) > 0 {
		d.Users = users
	}
	if len(teams.Upsert)+len(teams.Remove) > 0 {
		d.Teams = teams
	}
	if len(emails.Upsert)+len(emails.Remove) > 0 {
		d.Emails = emails
	}
	if t.restAt > base {
		r := rest(c)
		d.Rest = &r
	}
	return d
}
