package mgmt

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/audit"
	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/console"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// accessEnv is the real mux, console sign-in routes included, on a
// disposable database:
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/mgmt
type accessEnv struct {
	t   *testing.T
	ctx context.Context
	st  *store.Store
	m   *Server
	mux *http.ServeMux
	run string
}

const accessKey = "test-encryption-key-0123456789"

func newAccessEnv(t *testing.T) *accessEnv {
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
	if err := usage.NewPGWriter(st.Pool).EnsurePartitions(ctx); err != nil {
		t.Fatal(err)
	}
	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{})
	m := &Server{Store: st, AdminToken: "test-admin-token", EncryptionKey: accessKey, Snapshots: holder,
		Audit: &audit.Recorder{Pool: st.Pool}}
	mux := http.NewServeMux()
	m.Register(mux)
	(&console.Auth{Store: st, EncryptionKey: accessKey}).Register(mux)
	return &accessEnv{t: t, ctx: ctx, st: st, m: m, mux: mux, run: uuid.NewString()[:8]}
}

func (e *accessEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.st.Pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *accessEnv) user(role string) string {
	e.t.Helper()
	id := uuid.NewString()
	hash, _ := console.HashPassword("the-old-password")
	e.exec(`INSERT INTO users (id, email, name, role, password_hash) VALUES ($1,$2,'u',$3,$4)`,
		id, role+"-"+id[:8]+"@example.com", role, hash)
	e.t.Cleanup(func() { e.st.Pool.Exec(e.ctx, `DELETE FROM users WHERE id=$1`, id) })
	return id
}

// signIn is a fresh session cookie at the user's current epoch, as a
// sign-in would issue it.
func (e *accessEnv) signIn(userID string) *http.Cookie {
	e.t.Helper()
	var epoch int
	if err := e.st.Pool.QueryRow(e.ctx, `SELECT session_epoch FROM users WHERE id=$1`, userID).Scan(&epoch); err != nil {
		e.t.Fatal(err)
	}
	w := httptest.NewRecorder()
	console.SetSessionCookie(w, httptest.NewRequest("GET", "/", nil), accessKey, "", userID, epoch)
	return sessionFrom(e.t, w)
}

func sessionFrom(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == "llmr_session" {
			return c
		}
	}
	return nil
}

// do sends a request as "admin" (the bootstrap token), a session cookie, or
// nobody (nil).
func (e *accessEnv) do(method, path, body string, as any) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	switch v := as.(type) {
	case string:
		req.Header.Set("Authorization", "Bearer test-admin-token")
	case *http.Cookie:
		req.AddCookie(v)
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, req)
	return w
}

func (e *accessEnv) signedIn(c *http.Cookie) bool {
	e.t.Helper()
	switch code := e.do("GET", "/api/me", "", c).Code; code {
	case 200:
		return true
	case 401:
		return false
	default:
		e.t.Fatalf("GET /api/me: %d", code)
		return false
	}
}

// TestUsageRowScope: totals cover every team a member is in; individual
// requests only their own and those of teams they administer.
func TestUsageRowScope(t *testing.T) {
	e := newAccessEnv(t)
	team := uuid.NewString()
	e.exec(`INSERT INTO teams (id, name) VALUES ($1,$2)`, team, "scope-"+e.run)
	t.Cleanup(func() { e.st.Pool.Exec(e.ctx, `DELETE FROM teams WHERE id=$1`, team) })
	member, other, teamAdmin, outsider := e.user("member"), e.user("member"), e.user("member"), e.user("member")
	for uid, role := range map[string]string{member: "member", other: "member", teamAdmin: "admin"} {
		e.exec(`INSERT INTO team_members (team_id, user_id, role) VALUES ($1,$2,$3)`, team, uid, role)
	}

	// mine: the member's team request; theirs: a teammate's team request;
	// personal: the teammate's own, outside the team; lead: the team admin's.
	reqs := map[string]string{}
	var events []usage.Event
	for name, ev := range map[string]usage.Event{
		"mine":     {UserID: member, TeamID: team},
		"theirs":   {UserID: other, TeamID: team},
		"personal": {UserID: other},
		"lead":     {UserID: teamAdmin, TeamID: team},
	} {
		ev.TS, ev.RequestID, ev.ModelName, ev.StatusCode = time.Now().Add(-10*time.Minute), "req_"+name+"_"+e.run, "m", 200
		ev.PromptTokens, ev.CompletionTokens, ev.CostUSD, ev.Attempts = 10, 5, 0.01, 1
		reqs[name] = ev.RequestID
		events = append(events, ev)
	}
	tx, err := e.st.Pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := usage.InsertEvents(e.ctx, tx, events); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}

	cookies := map[string]any{"admin": "admin", "member": e.signIn(member), "team admin": e.signIn(teamAdmin),
		"outsider": e.signIn(outsider)}
	seen := func(as any, path string) map[string]bool {
		t.Helper()
		w := e.do("GET", path, "", as)
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		var rows []map[string]any
		json.Unmarshal(w.Body.Bytes(), &rows)
		out := map[string]bool{}
		for name, id := range reqs {
			for _, r := range rows {
				if r["request_id"] == id {
					out[name] = true
				}
			}
		}
		return out
	}
	want := map[string][]string{
		"admin":      {"lead", "mine", "personal", "theirs"},
		"member":     {"mine"},
		"team admin": {"lead", "mine", "theirs"},
		"outsider":   {},
	}
	for who, names := range want {
		for _, path := range []string{"/api/usage/requests?days=1&limit=500", "/api/usage/recent?limit=500"} {
			got := seen(cookies[who], path)
			if len(got) != len(names) {
				t.Errorf("%s %s: sees %v, want %v", who, path, got, names)
			}
			for _, n := range names {
				if !got[n] {
					t.Errorf("%s %s: missing %s (sees %v)", who, path, n, got)
				}
			}
		}
	}

	summary := func(as any, q string) float64 {
		t.Helper()
		w := e.do("GET", "/api/usage/summary?days=1&"+q, "", as)
		if w.Code != 200 {
			t.Fatalf("summary %s: %d %s", q, w.Code, w.Body.String())
		}
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		n, _ := out["requests"].(float64)
		return n
	}
	// One request's aggregate reads its raw events: scoped like the rows.
	for who, n := range map[string]float64{"admin": 1, "member": 0, "team admin": 1, "outsider": 0} {
		if got := summary(cookies[who], "request_id="+reqs["theirs"]); got != n {
			t.Errorf("%s: aggregate of a teammate's request = %v, want %v", who, got, n)
		}
	}
	// Totals without a request id keep the wider scope: the whole team.
	if got := summary(cookies["member"], "team_id="+team); got != 3 {
		t.Errorf("member: team total = %v, want 3", got)
	}
}

// TestSessionRevocation: sign-out, sign out everywhere, password changes and
// disabling end sessions on the server, not just in one browser.
func TestSessionRevocation(t *testing.T) {
	e := newAccessEnv(t)
	u := e.user("member")

	// A cookie from before sessions had ids still works, at epoch 0.
	legacy := &http.Cookie{Name: "llmr_session",
		Value: auth.SignToken(auth.DeriveKey("llmr-session", accessKey), u, time.Now().Add(time.Hour))}
	a, b := e.signIn(u), e.signIn(u)
	for _, c := range []*http.Cookie{legacy, a, b} {
		if !e.signedIn(c) {
			t.Fatal("fresh session refused")
		}
	}

	// Signing out revokes that session only, until its cookie would expire.
	if w := e.do("GET", "/auth/logout", "", a); w.Code != http.StatusFound {
		t.Fatalf("logout: %d", w.Code)
	}
	if e.signedIn(a) {
		t.Error("signed-out session still accepted")
	}
	if !e.signedIn(b) || !e.signedIn(legacy) {
		t.Error("signing out one session ended others")
	}
	var exp time.Time
	if err := e.st.Pool.QueryRow(e.ctx, `SELECT expires_at FROM revoked_sessions ORDER BY expires_at DESC LIMIT 1`).
		Scan(&exp); err != nil || exp.Before(time.Now().Add(11*time.Hour)) {
		t.Errorf("revoked session expiry = %v, %v; want the cookie's (12h)", exp, err)
	}

	// A legacy cookie has no id to revoke: signing it out ends them all.
	e.do("GET", "/auth/logout", "", legacy)
	if e.signedIn(legacy) || e.signedIn(b) {
		t.Error("signing out a legacy session left sessions alive")
	}

	// Changing your own password keeps you signed in and nobody else.
	mine, elsewhere := e.signIn(u), e.signIn(u)
	w := e.do("POST", "/api/me/password", `{"current":"the-old-password","new":"the-new-password"}`, mine)
	if w.Code != http.StatusNoContent {
		t.Fatalf("change password: %d %s", w.Code, w.Body.String())
	}
	reissued := sessionFrom(t, w)
	if reissued == nil || !e.signedIn(reissued) {
		t.Fatal("own session not carried over the password change")
	}
	if e.signedIn(mine) || e.signedIn(elsewhere) {
		t.Error("sessions from before the password change still accepted")
	}

	// An admin's reset ends them too.
	if w := e.do("POST", "/api/users/"+u+"/password", `{"password":"reset-by-admin"}`, "admin"); w.Code != http.StatusNoContent {
		t.Fatalf("admin reset: %d", w.Code)
	}
	if e.signedIn(reissued) {
		t.Error("session survived an admin password reset")
	}

	// Disabling ends sessions for good: enabling again does not revive them.
	c := e.signIn(u)
	e.do("PATCH", "/api/users/"+u, `{"disabled":true}`, "admin")
	e.do("PATCH", "/api/users/"+u, `{"disabled":false}`, "admin")
	if e.signedIn(c) {
		t.Error("session survived disable and enable")
	}

	// Sign out everywhere, by an admin and by the user.
	c = e.signIn(u)
	if w := e.do("POST", "/api/users/"+u+"/sign-out", "", "admin"); w.Code != http.StatusNoContent {
		t.Fatalf("admin sign-out: %d", w.Code)
	}
	if e.signedIn(c) {
		t.Error("session survived admin sign-out")
	}
	if w := e.do("POST", "/api/users/"+uuid.NewString()+"/sign-out", "", "admin"); w.Code != http.StatusNotFound {
		t.Errorf("sign-out of no one: %d", w.Code)
	}
	here, there := e.signIn(u), e.signIn(u)
	if w := e.do("POST", "/api/users/"+u+"/sign-out", "", here); w.Code != http.StatusForbidden {
		t.Errorf("member signing out a user by id: %d", w.Code)
	}
	w = e.do("POST", "/api/me/sign-out", "", here)
	if w.Code != http.StatusNoContent {
		t.Fatalf("self sign-out: %d", w.Code)
	}
	if c := sessionFrom(t, w); c == nil || c.MaxAge >= 0 {
		t.Error("self sign-out did not clear the cookie")
	}
	if e.signedIn(here) || e.signedIn(there) {
		t.Error("session survived sign out everywhere")
	}

	e.exec(`INSERT INTO revoked_sessions (sid, expires_at) VALUES ($1, now() - interval '1 minute')`, "stale-"+e.run)
	if err := console.PruneSessions(e.ctx, e.st.Pool); err != nil {
		t.Fatal(err)
	}
	var n int
	e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM revoked_sessions WHERE expires_at <= now()`).Scan(&n)
	if n != 0 {
		t.Errorf("%d expired revocations left after pruning", n)
	}
}

// TestKeyRotation: a rotated key's old secret works through its grace period
// and not after, without waiting for a snapshot rebuild.
func TestKeyRotation(t *testing.T) {
	e := newAccessEnv(t)
	owner, stranger := e.user("member"), e.user("member")
	ownerC, strangerC := e.signIn(owner), e.signIn(stranger)

	w := e.do("POST", "/api/keys", `{"name":"rot-`+e.run+`","user_id":"`+owner+`"}`, ownerC)
	if w.Code != http.StatusCreated {
		t.Fatalf("create key: %d %s", w.Code, w.Body.String())
	}
	var created struct{ ID, Key string }
	json.Unmarshal(w.Body.Bytes(), &created)
	id := created.ID
	secrets := []string{created.Key}

	rotate := func(as any, body string) (int, string, *time.Time) {
		t.Helper()
		w := e.do("POST", "/api/keys/"+id+"/rotate", body, as)
		var out struct {
			Key   string     `json:"key"`
			Until *time.Time `json:"old_key_expires_at"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code == 200 {
			secrets = append(secrets, out.Key)
		}
		return w.Code, out.Key, out.Until
	}
	build := func() *snapshot.Snapshot {
		t.Helper()
		s, err := (&snapshot.Builder{Pool: e.st.Pool, EncryptionKey: accessKey}).Build(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	works := func(s *snapshot.Snapshot, secret string) bool {
		id := auth.ResolveKey(s, secret)
		return id != nil && id.Key.ID == created.ID
	}
	oldHashes := func() int {
		var n int
		e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM api_key_old_hashes WHERE key_id=$1`, id).Scan(&n)
		return n
	}

	if code, _, _ := rotate(strangerC, `{"grace_hours":1}`); code != http.StatusNotFound {
		t.Errorf("stranger rotating: %d", code)
	}
	if code, _, _ := rotate(ownerC, `{"grace_hours":721}`); code != http.StatusBadRequest {
		t.Errorf("grace 721: %d", code)
	}
	code, s1, until := rotate(ownerC, `{"grace_hours":1}`)
	if code != 200 || !strings.HasPrefix(s1, auth.KeyPrefix) || until == nil ||
		until.Sub(time.Now()) < 58*time.Minute || until.Sub(time.Now()) > 62*time.Minute {
		t.Fatalf("rotate: %d %q until %v", code, s1, until)
	}
	snap := build()
	if !works(snap, secrets[0]) || !works(snap, s1) {
		t.Fatal("old and new secret must both work during the grace period")
	}
	var k *snapshot.Key
	h := sha256Of(secrets[0])
	if k = snap.KeysByHash[h]; k == nil || !k.OldHash || k.ExpiresAt == nil || !k.ExpiresAt.Equal(*until) {
		t.Fatalf("old entry = %+v", k)
	}

	// Again during the grace period (the default 24h): the first old secret
	// keeps its own expiry.
	code, s2, until2 := rotate(ownerC, ``)
	if code != 200 || until2 == nil || until2.Sub(time.Now()) < 23*time.Hour {
		t.Fatalf("second rotate: %d until %v", code, until2)
	}
	snap = build()
	for i, s := range []string{secrets[0], s1, s2} {
		if !works(snap, s) {
			t.Errorf("secret %d stopped working on a second rotation", i)
		}
	}
	if exp := snap.KeysByHash[h].ExpiresAt; exp == nil || !exp.Equal(*until) {
		t.Errorf("first old secret's expiry moved to %v", exp)
	}
	if n := oldHashes(); n != 2 {
		t.Errorf("%d old hashes, want 2", n)
	}

	// The key list says so.
	w = e.do("GET", "/api/keys?limit=50&q=rot-"+e.run, "", ownerC)
	var page struct {
		Items []map[string]any `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Items) != 1 || page.Items[0]["old_key_expires_at"] == nil {
		t.Errorf("key list: %s", w.Body.String())
	}

	// The key's own expiry wins when it comes first.
	soon := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	e.exec(`UPDATE api_keys SET expires_at=$2 WHERE id=$1`, id, soon)
	snap = build()
	if exp := snap.KeysByHash[sha256Of(s1)].ExpiresAt; exp == nil || !exp.Equal(soon) {
		t.Errorf("old secret outlives its key: %v, key expires %v", exp, soon)
	}
	e.exec(`UPDATE api_keys SET expires_at=NULL WHERE id=$1`, id)

	// Ends with the grace period, on the snapshot already built.
	e.exec(`UPDATE api_key_old_hashes SET expires_at = now() + interval '1 second' WHERE key_id=$1`, id)
	snap = build()
	if !works(snap, s1) {
		t.Fatal("old secret refused before its grace ended")
	}
	time.Sleep(1500 * time.Millisecond)
	if works(snap, s1) || works(snap, secrets[0]) {
		t.Error("old secret accepted after its grace ended")
	}
	if !works(snap, s2) {
		t.Error("current secret refused")
	}
	if snap = build(); works(snap, s1) {
		t.Error("expired old secret loaded again")
	}
	if err := PruneKeyHashes(e.ctx, e.st.Pool); err != nil {
		t.Fatal(err)
	}
	if n := oldHashes(); n != 0 {
		t.Errorf("%d old hashes left after pruning", n)
	}

	// Disabled keys serve no old secrets.
	rotate("admin", `{"grace_hours":24}`)
	e.exec(`UPDATE api_keys SET disabled=true WHERE id=$1`, id)
	if snap = build(); works(snap, s2) {
		t.Error("disabled key's old secret served")
	}
	e.exec(`UPDATE api_keys SET disabled=false WHERE id=$1`, id)

	// Grace 0 cuts the old secrets off at once, earlier ones included.
	code, s4, until4 := rotate(ownerC, `{"grace_hours":0}`)
	if code != 200 || until4 != nil {
		t.Fatalf("rotate now: %d until %v", code, until4)
	}
	snap = build()
	for i, s := range secrets[:len(secrets)-1] {
		if works(snap, s) {
			t.Errorf("secret %d still works after rotating with no grace", i)
		}
	}
	if !works(snap, s4) || oldHashes() != 0 {
		t.Error("rotation with no grace left old hashes")
	}

	// The audit row keeps the grace period and no secret.
	var details []string
	rows, err := e.st.Pool.Query(e.ctx, `SELECT coalesce(detail::text, '') FROM audit_events
		WHERE action='POST /api/keys/{id}/rotate' AND target_id=$1 AND status=200 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var d string
		rows.Scan(&d)
		details = append(details, d)
	}
	if strings.Join(details, " ") != `{"grace_hours": 1}  {"grace_hours": 24} {"grace_hours": 0}` {
		t.Errorf("audit details = %q", details)
	}
	for _, s := range secrets {
		var n int
		e.st.Pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_events WHERE coalesce(detail::text, '') LIKE '%' || $1 || '%'`,
			s[len(auth.KeyPrefix):]).Scan(&n)
		if n > 0 {
			t.Error("a key secret reached the audit log")
		}
	}

	// Deleting the key takes its old hashes with it.
	rotate(ownerC, `{"grace_hours":24}`)
	if w := e.do("DELETE", "/api/keys/"+id, "", ownerC); w.Code != http.StatusNoContent {
		t.Fatalf("delete key: %d", w.Code)
	}
	if n := oldHashes(); n != 0 {
		t.Errorf("%d old hashes outlived their key", n)
	}
}

func sha256Of(s string) [32]byte { return sha256.Sum256([]byte(s)) }
