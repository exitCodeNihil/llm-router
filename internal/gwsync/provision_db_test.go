package gwsync

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/limits"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
)

// testIssuer serves OIDC discovery and a JWKS with one RSA key, and signs
// tokens with any key under that key's kid.
type testIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	// keysDown makes the JWKS endpoint fail.
	keysDown atomic.Bool
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ti := &testIssuer{key: key}
	mux := http.NewServeMux()
	ti.srv = httptest.NewServer(mux)
	t.Cleanup(ti.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": ti.srv.URL, "jwks_uri": ti.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		if ti.keysDown.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	return ti
}

func (ti *testIssuer) sign(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	base := map[string]any{"iss": ti.srv.URL, "aud": "llmr", "exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(), "tid": "tenant"}
	for k, v := range claims {
		base[k] = v
	}
	payload, _ := json.Marshal(base)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := sig.CompactSerialize()
	return out
}

// TestProvisionAgainstPostgres: a gateway can create the user behind a valid
// identity token, once, and nobody else; it needs LLMR_TEST_DATABASE_URL.
func TestProvisionAgainstPostgres(t *testing.T) {
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

	idp := newTestIssuer(t)
	groupID, teamA, teamB := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, team := range []string{teamA, teamB} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO teams (id, name) VALUES ($1,$2)`, team, "team-"+team); err != nil {
			t.Fatal(err)
		}
	}
	token := JoinTokenPrefix + "prov-" + groupID
	hash := sha256.Sum256([]byte(token))
	if _, err := st.Pool.Exec(ctx, `INSERT INTO gateway_groups (id, name, allowed_teams, join_token_hash) VALUES ($1,$2,$3,$4)`,
		groupID, "prov-"+groupID, []string{teamA}, hash[:]); err != nil {
		t.Fatal(err)
	}
	issuer := &snapshot.Issuer{ID: "i1", Type: "oidc", IssuerURL: idp.srv.URL, Audience: "llmr", EmailClaim: "email",
		Match: map[string]string{"tid": "tenant"}, MapToTeam: teamA, AutoCreateUser: true}
	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{
		Version:       1,
		KeysByHash:    map[[32]byte]*snapshot.Key{},
		UsersByID:     map[string]*snapshot.User{},
		UsersByEmail:  map[string]*snapshot.User{},
		TeamsByID:     map[string]*snapshot.Team{teamA: {ID: teamA}, teamB: {ID: teamB}},
		Issuers:       []*snapshot.Issuer{issuer},
		GatewayGroups: map[string]*snapshot.GatewayGroup{groupID: {ID: groupID, Name: "prov", AllowedTeams: map[string]bool{teamA: true}}},
	})
	s := &Server{Store: st, Snapshots: holder, Limits: limits.New(), Key: auth.DeriveKey("llmr-gw-session", "test")}
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(path, bearer string, body any) (int, []byte) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+bearer)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out bytes.Buffer
		out.ReadFrom(res.Body)
		return res.StatusCode, out.Bytes()
	}
	code, body := post("/gw/v1/join", token, map[string]any{"name": "gw", "schema": 2})
	if code != 200 {
		t.Fatalf("join: %d %s", code, body)
	}
	var joined sessionResponse
	json.Unmarshal(body, &joined)
	version := func() (v int64) {
		st.Pool.QueryRow(ctx, `SELECT (value::text)::bigint FROM settings WHERE key='config_version'`).Scan(&v)
		return v
	}
	users := func(email string) (n int) {
		st.Pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email=$1`, email).Scan(&n)
		return n
	}
	email := "new-" + groupID + "@example.com"

	if code, _ := post("/gw/v1/provision", "not-a-session", map[string]string{"token": "x"}); code != 401 {
		t.Fatalf("no session: %d", code)
	}
	// A gateway can't mint a user: a token it signed itself, under the
	// issuer's name and kid, is refused, as is one for another audience.
	forger, _ := rsa.GenerateKey(rand.Reader, 2048)
	for name, tok := range map[string]string{
		"forged":         idp.sign(t, forger, map[string]any{"email": email}),
		"wrong audience": idp.sign(t, idp.key, map[string]any{"email": email, "aud": "other"}),
		"wrong tenant":   idp.sign(t, idp.key, map[string]any{"email": email, "tid": "evil"}),
		"expired":        idp.sign(t, idp.key, map[string]any{"email": email, "exp": time.Now().Add(-time.Minute).Unix()}),
	} {
		if code, body := post("/gw/v1/provision", joined.Session, map[string]string{"token": tok}); code != 403 {
			t.Errorf("%s token: %d %s", name, code, body)
		}
	}
	if users(email) != 0 {
		t.Fatal("a refused token created a user")
	}

	before := version()
	good := idp.sign(t, idp.key, map[string]any{"email": email})
	var first ProvisionResponse
	for i := range 2 { // the snapshot doesn't have the user yet the second time either
		code, body := post("/gw/v1/provision", joined.Session, map[string]string{"token": good})
		if code != 200 {
			t.Fatalf("provision %d: %d %s", i, code, body)
		}
		var got ProvisionResponse
		json.Unmarshal(body, &got)
		if i == 0 {
			first = got
		} else if got.UserID != first.UserID {
			t.Fatalf("second provision made another user: %s vs %s", got.UserID, first.UserID)
		}
	}
	if users(email) != 1 {
		t.Fatalf("users with %s = %d, want 1", email, users(email))
	}
	if len(first.TeamIDs) != 1 || first.TeamIDs[0] != teamA {
		t.Fatalf("team ids = %v, want the issuer's team", first.TeamIDs)
	}
	if got := version(); got != before+1 {
		t.Fatalf("config version %d -> %d: a new user must bump it once", before, got)
	}

	// A disabled account stays disabled.
	st.Pool.Exec(ctx, `UPDATE users SET disabled=true WHERE id=$1`, first.UserID)
	if code, body := post("/gw/v1/provision", joined.Session, map[string]string{"token": good}); code != 403 {
		t.Fatalf("disabled user: %d %s", code, body)
	}

	// A group whose teams new users don't join can't create them.
	issuer.MapToTeam = teamB
	other := idp.sign(t, idp.key, map[string]any{"email": "b-" + email})
	if code, body := post("/gw/v1/provision", joined.Session, map[string]string{"token": other}); code != 403 {
		t.Fatalf("team B user from team A's group: %d %s", code, body)
	}
	if users("b-"+email) != 0 {
		t.Fatal("a group created a user it doesn't serve")
	}

	// Provisioning is rate-limited per gateway.
	limited := false
	for range 2 * provisionPerMinute {
		if code, _ := post("/gw/v1/provision", joined.Session, map[string]string{}); code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("provisioning was never rate-limited")
	}

	st.Pool.Exec(ctx, `DELETE FROM users WHERE email=$1`, email)
	st.Pool.Exec(ctx, `DELETE FROM gateway_groups WHERE id=$1`, groupID)
	st.Pool.Exec(ctx, `DELETE FROM teams WHERE id = ANY($1)`, []string{teamA, teamB})
}

// provisionEnv is a control plane with one gateway group limited to teamA, an
// issuer that maps new users to teamA, and a joined gateway's session.
type provisionEnv struct {
	t                     *testing.T
	ctx                   context.Context
	st                    *store.Store
	s                     *Server
	idp                   *testIssuer
	issuer                *snapshot.Issuer
	srv                   *httptest.Server
	groupID, teamA, teamB string
	session               string
	instance              string
}

func newProvisionEnv(t *testing.T) *provisionEnv {
	t.Helper()
	url := os.Getenv("LLMR_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LLMR_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	e := &provisionEnv{t: t, ctx: ctx, st: st, idp: newTestIssuer(t),
		groupID: uuid.NewString(), teamA: uuid.NewString(), teamB: uuid.NewString()}
	for _, team := range []string{e.teamA, e.teamB} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO teams (id, name) VALUES ($1,$2)`, team, "team-"+team); err != nil {
			t.Fatal(err)
		}
	}
	token := JoinTokenPrefix + "prov-" + e.groupID
	hash := sha256.Sum256([]byte(token))
	if _, err := st.Pool.Exec(ctx, `INSERT INTO gateway_groups (id, name, allowed_teams, join_token_hash) VALUES ($1,$2,$3,$4)`,
		e.groupID, "prov-"+e.groupID, []string{e.teamA}, hash[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Pool.Exec(ctx, `DELETE FROM users WHERE email LIKE $1`, "%-"+e.groupID+"@example.com")
		st.Pool.Exec(ctx, `DELETE FROM gateway_groups WHERE id=$1`, e.groupID)
		st.Pool.Exec(ctx, `DELETE FROM teams WHERE id = ANY($1)`, []string{e.teamA, e.teamB})
	})
	e.issuer = &snapshot.Issuer{ID: "i1", Type: "oidc", IssuerURL: e.idp.srv.URL, Audience: "llmr", EmailClaim: "email",
		Match: map[string]string{"tid": "tenant"}, MapToTeam: e.teamA, AutoCreateUser: true}
	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{
		Version:      1,
		KeysByHash:   map[[32]byte]*snapshot.Key{},
		UsersByID:    map[string]*snapshot.User{},
		UsersByEmail: map[string]*snapshot.User{},
		TeamsByID:    map[string]*snapshot.Team{e.teamA: {ID: e.teamA}, e.teamB: {ID: e.teamB}},
		Issuers:      []*snapshot.Issuer{e.issuer},
		GatewayGroups: map[string]*snapshot.GatewayGroup{e.groupID: {ID: e.groupID, Name: "prov",
			AllowedTeams: map[string]bool{e.teamA: true}}},
	})
	e.s = &Server{Store: st, Snapshots: holder, Limits: limits.New(), Key: auth.DeriveKey("llmr-gw-session", "test")}
	mux := http.NewServeMux()
	e.s.Register(mux)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	code, body := e.post("/gw/v1/join", token, map[string]any{"name": "gw", "schema": 2})
	if code != 200 {
		t.Fatalf("join: %d %s", code, body)
	}
	var joined sessionResponse
	json.Unmarshal(body, &joined)
	e.session, e.instance = joined.Session, joined.InstanceID
	return e
}

func (e *provisionEnv) post(path, bearer string, body any) (int, []byte) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+bearer)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Error(err)
		return 0, nil
	}
	defer res.Body.Close()
	var out bytes.Buffer
	out.ReadFrom(res.Body)
	return res.StatusCode, out.Bytes()
}

func (e *provisionEnv) email(name string) string { return name + "-" + e.groupID + "@example.com" }

// TestProvisionConcurrentFirstCalls: several gateways (or requests) provisioning
// one new person at once all get the same user, already in the issuer's team.
func TestProvisionConcurrentFirstCalls(t *testing.T) {
	e := newProvisionEnv(t)
	v := auth.NewCloudValidator(e.st.Pool)
	for round := range 5 {
		email := e.email(fmt.Sprintf("race%d", round))
		var wg sync.WaitGroup
		got := make([]*snapshot.User, 8)
		errs := make([]error, len(got))
		for i := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got[i], errs[i] = v.ProvisionUser(e.ctx, email, e.teamA)
			}()
		}
		wg.Wait()
		for i, u := range got {
			if errs[i] != nil {
				t.Fatalf("round %d caller %d: %v", round, i, errs[i])
			}
			if u.ID != got[0].ID {
				t.Fatalf("round %d: two users for one email: %s, %s", round, u.ID, got[0].ID)
			}
			if !slices.Equal(u.TeamIDs, []string{e.teamA}) {
				t.Fatalf("round %d caller %d: team ids %v, want [%s]", round, i, u.TeamIDs, e.teamA)
			}
		}
	}
}

// TestProvisionJoinFailureCreatesNoUser: a user is never left without the team
// their issuer maps them to.
func TestProvisionJoinFailureCreatesNoUser(t *testing.T) {
	e := newProvisionEnv(t)
	v := auth.NewCloudValidator(e.st.Pool)
	email := e.email("nojoin")
	if _, err := v.ProvisionUser(e.ctx, email, uuid.NewString()); err == nil {
		t.Fatal("provisioning into a team that does not exist succeeded")
	}
	var n int
	e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM users WHERE email=$1`, email).Scan(&n)
	if n != 0 {
		t.Fatal("a failed team join left the user behind")
	}
	u, err := v.ProvisionUser(e.ctx, email, e.teamA)
	if err != nil || !slices.Equal(u.TeamIDs, []string{e.teamA}) {
		t.Fatalf("retry: %+v %v", u, err)
	}
}

// TestProvisionExistingNonMember: an account that exists but is in none of the
// group's teams is refused, and provisioning doesn't add it to one.
func TestProvisionExistingNonMember(t *testing.T) {
	e := newProvisionEnv(t)
	email := e.email("existing")
	id := uuid.NewString()
	if _, err := e.st.Pool.Exec(e.ctx, `INSERT INTO users (id, email, role) VALUES ($1,$2,'member')`, id, email); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Pool.Exec(e.ctx, `INSERT INTO team_members (team_id, user_id, role) VALUES ($1,$2,'member')`, e.teamB, id); err != nil {
		t.Fatal(err)
	}
	tok := e.idp.sign(t, e.idp.key, map[string]any{"email": email})
	if code, body := e.post("/gw/v1/provision", e.session, map[string]string{"token": tok}); code != http.StatusForbidden {
		t.Fatalf("existing user outside the group: %d %s", code, body)
	}
	var teams []string
	rows, _ := e.st.Pool.Query(e.ctx, `SELECT team_id::text FROM team_members WHERE user_id=$1`, id)
	teams, _ = pgx.CollectRows(rows, pgx.RowTo[string])
	if !slices.Equal(teams, []string{e.teamB}) {
		t.Fatalf("provisioning changed an existing account's teams: %v", teams)
	}
}

// TestProvisionIssuerUnavailable: the control plane failing to reach the
// issuer is a 503 the gateway retries soon, not a refusal it keeps, and the
// answer names no URL.
func TestProvisionIssuerUnavailable(t *testing.T) {
	e := newProvisionEnv(t)
	e.idp.keysDown.Store(true)
	tok := e.idp.sign(t, e.idp.key, map[string]any{"email": e.email("down")})
	req, _ := http.NewRequest("POST", e.srv.URL+"/gw/v1/provision",
		strings.NewReader(`{"token":"`+tok+`"}`))
	req.Header.Set("Authorization", "Bearer "+e.session)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	body.ReadFrom(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") == "" {
		t.Fatalf("issuer keys down: %d (Retry-After %q) %s", res.StatusCode, res.Header.Get("Retry-After"), body.String())
	}
	if strings.Contains(body.String(), e.idp.srv.URL) {
		t.Fatalf("the answer names the issuer's URL: %s", body.String())
	}
	e.idp.keysDown.Store(false)
	if code, body := e.post("/gw/v1/provision", e.session, map[string]string{"token": tok}); code != 200 {
		t.Fatalf("issuer back: %d %s", code, body)
	}
	// A refusal says why in a word, without the detail.
	bad := e.idp.sign(t, e.idp.key, map[string]any{"email": e.email("down"), "aud": "other"})
	code, b := e.post("/gw/v1/provision", e.session, map[string]string{"token": bad})
	if code != http.StatusForbidden || strings.TrimSpace(string(b)) != "invalid identity token: wrong audience" {
		t.Fatalf("wrong audience: %d %s", code, b)
	}
}

// TestProvisionNeedsALiveSchema2Session: a 0.2.x session can't provision, nor
// can the session of an instance that has been removed.
func TestProvisionNeedsALiveSchema2Session(t *testing.T) {
	e := newProvisionEnv(t)
	tok := e.idp.sign(t, e.idp.key, map[string]any{"email": e.email("sess")})
	old := e.s.newSession(peer{instance: e.instance, group: e.groupID, schema: 1}).Session
	if code, body := e.post("/gw/v1/provision", old, map[string]string{"token": tok}); code != http.StatusBadRequest {
		t.Fatalf("schema 1 session: %d %s", code, body)
	}
	if _, err := e.st.Pool.Exec(e.ctx, `DELETE FROM gateway_instances WHERE id=$1`, e.instance); err != nil {
		t.Fatal(err)
	}
	if code, body := e.post("/gw/v1/provision", e.session, map[string]string{"token": tok}); code != http.StatusUnauthorized {
		t.Fatalf("removed instance: %d %s", code, body)
	}
	var n int
	e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM users WHERE email=$1`, e.email("sess")).Scan(&n)
	if n != 0 {
		t.Fatal("a refused session created a user")
	}
}
