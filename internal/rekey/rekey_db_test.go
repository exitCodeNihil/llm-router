package rekey

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/console"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
)

const keyA, keyB = "rotation-key-a-0123456789", "rotation-key-b-0123456789"

// The rotation stages from a to b: before, b as NEXT, b current with a as
// OLD, after.
var (
	stage0 = snapshot.Keyring{Current: keyA}
	stage1 = snapshot.Keyring{Current: keyA, Others: []string{keyB}}
	stage2 = snapshot.Keyring{Current: keyB, Others: []string{keyA}}
	stage3 = snapshot.Keyring{Current: keyB}
)

type fixture struct {
	t  *testing.T
	st *store.Store
	// the plaintext of every secret, by provider name, destination name,
	// file path and "sso"
	providers map[string]string
	dests     map[string]string
	files     map[string]string
	sso       string
	user      string
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.st.Pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// write stores one of each secret under keys' current key, as a replica on
// that stage would.
func (f *fixture) write(keys snapshot.Keyring, suffix string) {
	f.t.Helper()
	enc := func(s string) []byte {
		b, err := keys.Encrypt(s)
		if err != nil {
			f.t.Fatal(err)
		}
		return b
	}
	name, plain := "p-"+suffix, "sk-"+suffix
	f.exec(`INSERT INTO providers (id, name, type, base_url, auth_mode, api_key_enc)
		VALUES ($1, $2, 'openai_compatible', 'http://upstream.invalid', 'api_key', $3)`, store.NewID(), name, enc(plain))
	f.providers[name] = plain
	f.exec(`INSERT INTO model_deployments (id, provider_id, model_name, upstream_name)
		SELECT $1, id, $2, 'up' FROM providers WHERE name = $2`, store.NewID(), name)

	dname := "d-" + suffix
	secrets, _ := json.Marshal(map[string]string{"secret_key": "dk-" + suffix})
	f.exec(`INSERT INTO telemetry_destinations (id, name, kind, secrets_enc, secret_names)
		VALUES ($1, $2, 'otlp', $3, '{secret_key}')`, store.NewID(), dname, enc(string(secrets)))
	f.dests[dname] = "dk-" + suffix

	path := ".config/" + suffix
	f.exec(`INSERT INTO workspace_user_files (user_id, path, data_enc, size) VALUES ($1, $2, $3, 1)`,
		f.user, path, enc("file-"+suffix))
	f.files[path] = "file-" + suffix

	f.sso = "sso-" + suffix
	if err := console.SaveSSO(context.Background(), f.st.Pool, keys.Current, &console.SSOConfig{
		IssuerURL: "https://idp.invalid", ClientID: "cid", ClientSecret: f.sso, RedirectURL: "https://r.invalid/cb"}); err != nil {
		f.t.Fatal(err)
	}
}

// readsAll checks a replica with keys reads every secret: through a snapshot
// build, the SSO settings and the workspace files.
func (f *fixture) readsAll(keys snapshot.Keyring, label string) {
	f.t.Helper()
	ctx := context.Background()
	s, err := (&snapshot.Builder{Pool: f.st.Pool, EncryptionKey: keys.Current, OtherKeys: keys.Others}).Build(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	for name, want := range f.providers {
		ds := s.DeploymentsByModel[name]
		if len(ds) != 1 || ds[0].Provider.APIKey != want {
			f.t.Errorf("%s: provider %s not loaded with its key", label, name)
		}
	}
	got := map[string]string{}
	for _, d := range s.Destinations {
		if d.Err != "" {
			f.t.Errorf("%s: destination %s: %s", label, d.Name, d.Err)
		}
		got[d.Name] = d.Secrets["secret_key"]
	}
	for name, want := range f.dests {
		if got[name] != want {
			f.t.Errorf("%s: destination %s secret %q, want %q", label, name, got[name], want)
		}
	}
	cfg, err := console.LoadSSO(ctx, f.st.Pool, keys)
	if err != nil || cfg.ClientSecret != f.sso {
		f.t.Errorf("%s: SSO secret %+v %v", label, cfg, err)
	}
	rows, err := f.st.Pool.Query(ctx, `SELECT path, data_enc FROM workspace_user_files`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var path string
		var enc []byte
		if err := rows.Scan(&path, &enc); err != nil {
			f.t.Fatal(err)
		}
		if plain, err := keys.Decrypt(enc); err != nil || plain != f.files[path] {
			f.t.Errorf("%s: file %s: %q %v", label, path, plain, err)
		}
		n++
	}
	if n != len(f.files) {
		f.t.Errorf("%s: %d files, want %d", label, n, len(f.files))
	}
}

// signsAcross checks a console session and a gateway session signed by one
// replica verify on the other, both ways.
func signsAcross(t *testing.T, x, y snapshot.Keyring, label string) {
	t.Helper()
	for _, pair := range [][2]snapshot.Keyring{{x, y}, {y, x}} {
		signer, verifier := pair[0], pair[1]
		w := httptest.NewRecorder()
		console.SetSessionCookie(w, httptest.NewRequest(http.MethodGet, "/", nil), signer.Current, "", "user-1", 0)
		cookies := w.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("%s: %d cookies", label, len(cookies))
		}
		if s, err := console.VerifySession(verifier, cookies[0].Value); err != nil || s.UserID != "user-1" {
			t.Errorf("%s: session cookie: %+v %v", label, s, err)
		}
		// as internal/gwsync signs and verifies
		tok := auth.SignToken(auth.DeriveKey("llmr-gw-session", signer.Current), "inst:group:2", time.Now().Add(time.Hour))
		if sub, err := auth.VerifyTokenAny(auth.DeriveKeys("llmr-gw-session", verifier.All()...), tok); err != nil || sub != "inst:group:2" {
			t.Errorf("%s: gateway session: %q %v", label, sub, err)
		}
	}
}

func (f *fixture) ciphertexts() map[string][]byte {
	f.t.Helper()
	out := map[string][]byte{}
	rows, err := f.st.Pool.Query(context.Background(), `
		SELECT 'p/' || name, api_key_enc FROM providers
		UNION ALL SELECT 'd/' || name, secrets_enc FROM telemetry_destinations
		UNION ALL SELECT 'f/' || path, data_enc FROM workspace_user_files
		UNION ALL SELECT 'sso', convert_to(value->>'client_secret_enc', 'UTF8') FROM settings WHERE key = 'sso'`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			f.t.Fatal(err)
		}
		out[k] = v
	}
	return out
}

// TestRotationAgainstPostgres runs a rotation from key a to key b with two
// replicas a stage apart, as during each rollout.
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/rekey
func TestRotationAgainstPostgres(t *testing.T) {
	url := os.Getenv("LLMR_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LLMR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, st: st, providers: map[string]string{}, dests: map[string]string{}, files: map[string]string{},
		user: store.NewID().String()}
	f.exec(`INSERT INTO users (id, email, role) VALUES ($1, 'rotation@example.com', 'member')`, f.user)
	f.exec(`DELETE FROM settings WHERE key = 'sso'`)

	f.write(stage0, "s0")
	f.readsAll(stage0, "stage 0")

	// (a) NEXT rolls out: the stage-1 replica writes under a as before.
	f.write(stage1, "s1")
	for _, k := range []snapshot.Keyring{stage0, stage1} {
		f.readsAll(k, "rollout a")
	}
	signsAcross(t, stage0, stage1, "rollout a")

	// (b) b becomes current: the stage-2 replica writes under b, the stage-1
	// one still under a; each reads the other's.
	f.write(stage2, "s2")
	for _, k := range []snapshot.Keyring{stage1, stage2} {
		f.readsAll(k, "rollout b")
	}
	signsAcross(t, stage1, stage2, "rollout b")

	// While a stage-1 replica is alive nothing is rewritten: it reports a
	// different key.
	disagree := &Rekeyer{Pool: st.Pool, Keys: stage2, PeersAgree: func() bool { return false }}
	if n, err := disagree.Pass(ctx); err != nil || n != 0 {
		t.Fatalf("pass with peers disagreeing: %d %v", n, err)
	}
	// s0 and s1 secrets are under a, the SSO secret (last saved by stage 2) under b
	if got := disagree.OldKeyCount(); got != 6 {
		t.Errorf("old-key count %d, want 6", got)
	}
	// Nor while another replica holds the lock.
	held, err := st.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	held.Exec(ctx, `SELECT pg_advisory_lock($1)`, store.LockRekey)
	agree := &Rekeyer{Pool: st.Pool, Keys: stage2, PeersAgree: func() bool { return true }}
	if n, err := agree.Pass(ctx); err != nil || n != 0 || agree.OldKeyCount() != 6 {
		t.Fatalf("pass without the lock: %d %v, count %d", n, err, agree.OldKeyCount())
	}
	held.Exec(ctx, `SELECT pg_advisory_unlock($1)`, store.LockRekey)
	held.Release()

	// Every replica on stage 2: rewritten, but an admin's edit racing the
	// rewrite wins, and rows already under b are not touched.
	before := f.ciphertexts()
	var edited []byte
	agree.beforeWrite = func(ref string) {
		if edited != nil {
			return
		}
		f.providers["p-s0"] = "sk-edited"
		edited, _ = stage2.Encrypt("sk-edited")
		f.exec(`UPDATE providers SET api_key_enc = $1 WHERE name = 'p-s0'`, edited)
	}
	n, err := agree.Pass(ctx)
	if err != nil || n != 5 {
		t.Fatalf("pass: %d %v, want 5 rewritten (6 less the edited one)", n, err)
	}
	if agree.OldKeyCount() != 0 {
		t.Errorf("old-key count %d after the pass", agree.OldKeyCount())
	}
	after := f.ciphertexts()
	for k, v := range before {
		_, which, _ := stage2.Open(v)
		if which == 0 && !bytes.Equal(after[k], v) {
			t.Errorf("%s was under the current key and was rewritten", k)
		}
	}
	if !bytes.Equal(after["p/p-s0"], edited) {
		t.Error("the concurrent edit was overwritten")
	}
	f.readsAll(stage2, "after re-encryption")
	if n, err := agree.Pass(ctx); err != nil || n != 0 || !equal(f.ciphertexts(), after) {
		t.Errorf("second pass: %d %v, or changed rows", n, err)
	}

	// (c) OLD removed: the stage-3 replica reads everything with b alone,
	// alongside a stage-2 one.
	for _, k := range []snapshot.Keyring{stage2, stage3} {
		f.readsAll(k, "rollout c")
	}
	signsAcross(t, stage2, stage3, "rollout c")
	last := &Rekeyer{Pool: st.Pool, Keys: stage3, PeersAgree: func() bool { return true }}
	if n, err := last.Pass(ctx); err != nil || n != 0 || last.OldKeyCount() != 0 {
		t.Errorf("stage 3 pass: %d %v, count %d", n, err, last.OldKeyCount())
	}
}

func equal(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(b[k], v) {
			return false
		}
	}
	return true
}
