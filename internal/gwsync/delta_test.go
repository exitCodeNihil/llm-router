package gwsync

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/limits"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// apply is what a gateway does with a delta, done here to check that a
// patched config is the config.
func apply(t *testing.T, c Config, d *Delta) Config {
	t.Helper()
	if d.BaseVersion != c.Version || d.BaseDigest != c.Digest {
		t.Fatalf("delta is from v%d %s, config is v%d %s", d.BaseVersion, d.BaseDigest, c.Version, c.Digest)
	}
	out := c
	out.Version = d.Version
	if d.Keys != nil {
		gone := map[string]bool{}
		for _, h := range d.Keys.Remove {
			gone[h] = true
		}
		for _, k := range d.Keys.Upsert {
			gone[k.Hash] = true
		}
		out.Keys = nil
		for _, k := range c.Keys {
			if !gone[k.Hash] {
				out.Keys = append(out.Keys, k)
			}
		}
		out.Keys = append(out.Keys, d.Keys.Upsert...)
	}
	if d.Users != nil {
		gone := map[string]bool{}
		for _, id := range d.Users.Remove {
			gone[id] = true
		}
		for _, u := range d.Users.Upsert {
			gone[u.ID] = true
		}
		out.Users = nil
		for _, u := range c.Users {
			if !gone[u.ID] {
				out.Users = append(out.Users, u)
			}
		}
		out.Users = append(out.Users, d.Users.Upsert...)
	}
	if d.Teams != nil {
		gone := map[string]bool{}
		for _, id := range d.Teams.Remove {
			gone[id] = true
		}
		for _, tm := range d.Teams.Upsert {
			gone[tm.ID] = true
		}
		out.Teams = nil
		for _, tm := range c.Teams {
			if !gone[tm.ID] {
				out.Teams = append(out.Teams, tm)
			}
		}
		out.Teams = append(out.Teams, d.Teams.Upsert...)
	}
	if d.Emails != nil {
		out.Emails = map[string]string{}
		for k, v := range c.Emails {
			out.Emails[k] = v
		}
		for _, k := range d.Emails.Remove {
			delete(out.Emails, k)
		}
		for k, v := range d.Emails.Upsert {
			out.Emails[k] = v
		}
	}
	if d.Rest != nil {
		out.Group, out.Providers, out.Deployments, out.Prices = d.Rest.Group, d.Rest.Providers, d.Rest.Deployments, d.Rest.Prices
		out.Issuers, out.Telemetry, out.RestHash = d.Rest.Issuers, d.Rest.Telemetry, d.Rest.RestHash
	}
	// The digest from the entry hashes as received, never recomputed from content.
	var sum digest
	for _, k := range out.Keys {
		sum.add("keys", k.Hash, k.H)
	}
	for _, u := range out.Users {
		sum.add("users", u.ID, u.H)
	}
	for _, tm := range out.Teams {
		sum.add("teams", tm.ID, tm.H)
	}
	for k, v := range out.Emails {
		sum.add("emails", k, v)
	}
	sum.add("rest", "", out.RestHash)
	out.Digest = sum.String()
	if out.Digest != d.Digest {
		t.Fatalf("patched config has digest %s, the delta promised %s", out.Digest, d.Digest)
	}
	return out
}

func sealed(s *snapshot.Snapshot) Config {
	b := buildConfig(s, &snapshot.GatewayGroup{ID: "g", Name: "g"}, schemaVersion)
	seal(&b.cfg, "e1")
	return b.cfg
}

func TestDigestExample(t *testing.T) {
	// One key {hash "aa", h "01"}, rest hash "ff": the worked example the
	// gateway's tests use too.
	var d digest
	d.add("keys", "aa", "01")
	d.add("rest", "", "ff")
	var again digest
	again.add("rest", "", "ff")
	again.add("keys", "aa", "01")
	if d != again || len(d.String()) != 64 {
		t.Fatalf("digest depends on order: %s vs %s", d, again)
	}
	t.Logf("digest of {keys/aa/01, rest//ff} = %s", d)
}

func TestDeltaFollowsEveryKindOfChange(t *testing.T) {
	s := fixture()
	s.Issuers = []*snapshot.Issuer{{ID: "i", Type: "entra", IssuerURL: "https://issuer.example", Audience: "a"}}
	var tr tracker
	v7 := sealed(s)
	tr.advance(&v7)

	// v8: a key added, one removed, a user changed.
	s.Version = 8
	delete(s.KeysByHash, [32]byte{4})
	s.KeysByHash[[32]byte{9}] = &snapshot.Key{ID: "k-new", UserID: "bob", TeamID: "B"}
	rpm := 10
	s.UsersByID["alice"].RPMLimit = &rpm
	v8 := sealed(s)
	tr.advance(&v8)
	d := tr.since(&v8, 7, v7.Digest)
	if d == nil || d.Keys == nil || len(d.Keys.Upsert) != 1 || len(d.Keys.Remove) != 1 || d.Users == nil ||
		len(d.Users.Upsert) != 1 || d.Teams != nil || d.Rest != nil {
		t.Fatalf("v7→v8: %+v", d)
	}
	got := apply(t, v7, d)

	// v9: a rotated key (two secrets, one id), a team changed, a price changed,
	// a user gone with their email.
	s.Version = 9
	s.KeysByHash[[32]byte{10}] = &snapshot.Key{ID: "k-new", UserID: "bob", TeamID: "B", OldHash: true}
	s.TeamsByID["B"].RPMLimit = &rpm
	s.Prices["gpt-a"] = snapshot.Price{InputPer1M: 3, OutputPer1M: 4}
	delete(s.UsersByID, "carol")
	v9 := sealed(s)
	tr.advance(&v9)
	d = tr.since(&v9, 8, v8.Digest)
	if d == nil || d.Rest == nil || d.Teams == nil || d.Users == nil || len(d.Users.Remove) != 1 || d.Emails == nil {
		t.Fatalf("v8→v9: %+v", d)
	}
	got = apply(t, got, d)
	// Two versions at once, from the config a slower gateway still holds.
	if d = tr.since(&v9, 7, v7.Digest); d == nil {
		t.Fatal("no delta across two versions")
	}
	apply(t, v7, d)

	// v10: the old secret's grace ends. Only that entry goes; the key stays.
	s.Version = 10
	delete(s.KeysByHash, [32]byte{10})
	v10 := sealed(s)
	tr.advance(&v10)
	d = tr.since(&v10, 9, v9.Digest)
	if d == nil || len(d.Keys.Remove) != 1 || len(d.Keys.Upsert) != 0 {
		t.Fatalf("v9→v10: %+v", d.Keys)
	}
	got = apply(t, got, d)
	n := 0
	for _, k := range got.Keys {
		if k.ID == "k-new" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d entries left for the rotated key, want its current secret only", n)
	}

	// No delta: a version never built here, a digest that differs, a base
	// that is not older, a tracker that has moved on.
	for name, d := range map[string]*Delta{
		"unknown version": tr.since(&v10, 6, v7.Digest),
		"other digest":    tr.since(&v10, 9, v8.Digest),
		"no digest":       tr.since(&v10, 9, ""),
		"same version":    tr.since(&v10, 10, v10.Digest),
		"old head":        tr.since(&v9, 8, v8.Digest),
	} {
		if d != nil {
			t.Errorf("%s: got a delta", name)
		}
	}
}

func TestTrackerStartsOverPastItsMemory(t *testing.T) {
	s := fixture()
	var tr tracker
	v7 := sealed(s)
	tr.advance(&v7)
	for i := range maxRemoved + 1 {
		tr.removed[entryKey{"keys", strconv.Itoa(i)}] = 7
	}
	s.Version = 8
	v8 := sealed(s)
	tr.advance(&v8)
	if tr.since(&v8, 7, v7.Digest) != nil {
		t.Fatal("a delta from before the tracker started over")
	}
	s.Version = 9
	v9 := sealed(s)
	tr.advance(&v9)
	if tr.since(&v9, 8, v8.Digest) == nil {
		t.Fatal("no delta after starting over")
	}
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r)
	return out
}

// The config endpoint: a schema 3 gateway gets a delta when it can, a full
// config from another database whatever the version, nothing for an older
// version of the same one, and wakes as soon as the snapshot changes.
func TestConfigEndpointSchema3(t *testing.T) {
	s := fixture()
	s.Epoch = "e1"
	s.GatewayGroups = map[string]*snapshot.GatewayGroup{"g": {ID: "g", Name: "g"}}
	h := &snapshot.Holder{}
	h.Set(s)
	srv := &Server{Snapshots: h, Limits: limits.New()}
	get := func(schema int, etag, digest string, wait int) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/gw/v1/config?wait="+strconv.Itoa(wait), nil)
		if etag != "" {
			r.Header.Set("If-None-Match", etag)
		}
		if digest != "" {
			r.Header.Set("X-Llmr-Config-Digest", digest)
		}
		w := httptest.NewRecorder()
		srv.config(w, r, peer{instance: "i", group: "g", schema: schema})
		return w
	}
	full := get(3, "", "", 0)
	var c7 Config
	if err := json.Unmarshal(gunzip(t, full.Body.Bytes()), &c7); err != nil || c7.Digest == "" || c7.Keys[0].H == "" {
		t.Fatalf("full config: %v %+v", err, c7.Digest)
	}
	if full.Header().Get("ETag") != `"e1-v7"` {
		t.Fatalf("etag %s", full.Header().Get("ETag"))
	}
	// A schema 2 gateway gets no hashes and the old tag.
	old := get(2, "", "", 0)
	var c2 Config
	json.Unmarshal(gunzip(t, old.Body.Bytes()), &c2)
	if old.Header().Get("ETag") != `"v7"` || c2.Digest != "" || c2.Keys[0].H != "" || c2.Schema != 2 {
		t.Fatalf("schema 2: etag %s digest %q", old.Header().Get("ETag"), c2.Digest)
	}
	if w := get(3, `"e1-v7"`, c7.Digest, 0); w.Code != http.StatusNotModified {
		t.Fatalf("same version: %d", w.Code)
	}
	// A later version from another control plane is not replaced by this one's.
	if w := get(3, `"e1-v9"`, "x", 0); w.Code != http.StatusNotModified {
		t.Fatalf("this control plane is behind: %d", w.Code)
	}
	// Another database: its config, in full, though its version is lower.
	if w := get(3, `"other-v9"`, "x", 0); w.Code != http.StatusOK {
		t.Fatalf("restored database: %d", w.Code)
	}
	if w := get(3, `"other-v7"`, "x", 0); w.Code != http.StatusOK {
		t.Fatalf("another database on the same version number: %d", w.Code)
	}

	// A change wakes a waiting gateway at once, with a delta.
	s2 := fixture()
	s2.Epoch, s2.Version, s2.GatewayGroups = "e1", 8, s.GatewayGroups
	s2.KeysByHash[[32]byte{9}] = &snapshot.Key{ID: "k-new", UserID: "bob", TeamID: "B"}
	go func() {
		time.Sleep(50 * time.Millisecond)
		h.Set(s2)
	}()
	start := time.Now()
	w := get(3, `"e1-v7"`, c7.Digest, 20)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("waited %v for a change", time.Since(start))
	}
	var d Delta
	if err := json.Unmarshal(gunzip(t, w.Body.Bytes()), &d); err != nil || !d.Delta || d.Version != 8 || len(d.Keys.Upsert) != 1 {
		t.Fatalf("expected a delta: %v %s", err, gunzip(t, w.Body.Bytes()))
	}
	apply(t, c7, &d)
	// With a digest this control plane does not know: the whole config.
	w = get(3, `"e1-v7"`, "0000", 0)
	var c8 Config
	if json.Unmarshal(gunzip(t, w.Body.Bytes()), &c8); c8.Version != 8 || len(c8.Keys) == 0 || c8.Digest != d.Digest {
		t.Fatalf("expected the full config: v%d", c8.Version)
	}
}
