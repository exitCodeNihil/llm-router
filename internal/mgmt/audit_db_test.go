package mgmt

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/exitcodenihil/llm-router/internal/audit"
	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/console"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
)

// TestAuditAgainstPostgres sends secrets and personal data through the real
// mux and checks none of it reaches the audit log.
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/mgmt
func TestAuditAgainstPostgres(t *testing.T) {
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
	const key = "test-encryption-key-0123456789"
	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{})
	rec := &audit.Recorder{Pool: st.Pool}
	m := &Server{Store: st, AdminToken: "test-admin-token", EncryptionKey: key, Snapshots: holder, Audit: rec}
	mux := http.NewServeMux()
	m.Register(mux)

	// The SSO setting is global: put back whatever was there.
	var oldSSO []byte
	st.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='sso'`).Scan(&oldSSO)
	defer func() {
		if oldSSO == nil {
			st.Pool.Exec(ctx, `DELETE FROM settings WHERE key='sso'`)
		} else {
			st.Pool.Exec(ctx, `UPDATE settings SET value=$1 WHERE key='sso'`, oldSSO)
		}
	}()

	var start int64
	if err := st.Pool.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM audit_events`).Scan(&start); err != nil {
		t.Fatal(err)
	}
	run := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	sentinel := func(s string) string { return "sentinel-" + s + "-" + run }

	// A member signed in with a console session.
	memberID := uuid.New()
	memberEmail := "member-" + run + "@example.com"
	hash, _ := console.HashPassword("the-right-password")
	if _, err := st.Pool.Exec(ctx, `INSERT INTO users (id, email, name, role, password_hash) VALUES ($1,$2,'m','member',$3)`,
		memberID, memberEmail, hash); err != nil {
		t.Fatal(err)
	}
	defer st.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, memberID)
	session := &http.Cookie{Name: "llmr_session",
		Value: auth.SignToken(auth.DeriveKey("llmr-session", key), memberID.String(), time.Now().Add(time.Hour))}

	type call struct {
		method, path, body string
		ctype              string
		as                 string // "admin" (default), "member", "none", "bad-token"
	}
	do := func(c call) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		if c.ctype == "" {
			c.ctype = "application/json"
		}
		req.Header.Set("Content-Type", c.ctype)
		req.RemoteAddr = "192.0.2.7:4321"
		switch c.as {
		case "", "admin":
			req.Header.Set("Authorization", "Bearer test-admin-token")
		case "member":
			req.AddCookie(session)
		case "bad-token":
			req.Header.Set("Authorization", "Bearer "+sentinel("wrong-token"))
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	// Secrets handed back by the server once must not be recorded either.
	var issued []string
	code, out := do(call{method: "POST", path: "/api/users", body: `{"email":"` + sentinel("email") + `@example.com","name":"` +
		sentinel("name") + `","password":"` + sentinel("password") + `","role":"member","rpm_limit":7,"tags":["` + sentinel("tag") + `"]}`})
	if code != http.StatusCreated {
		t.Fatalf("create user: %d %v", code, out)
	}
	userID := out["id"].(string)
	defer st.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID)

	code, out = do(call{method: "POST", path: "/api/gateway-groups", body: `{"name":"g-` + run + `"}`})
	if code != http.StatusCreated {
		t.Fatalf("create group: %d %v", code, out)
	}
	groupID := out["id"].(string)
	defer st.Pool.Exec(ctx, `DELETE FROM gateway_groups WHERE id=$1`, groupID)
	issued = append(issued, out["join_token"].(string))

	var multi bytes.Buffer
	mw := multipart.NewWriter(&multi)
	fw, _ := mw.CreateFormFile("file", "id_ed25519")
	fw.Write([]byte(sentinel("private-key")))
	mw.Close()

	for _, c := range []call{
		{method: "POST", path: "/api/users/" + userID + "/password", body: `{"password":"` + sentinel("set-password") + `"}`},
		{method: "PATCH", path: "/api/users/" + userID, body: `{"name":"` + sentinel("rename") + `","tags":["` + sentinel("tag2") + `"],"rpm_limit":9}`},
		{method: "POST", path: "/api/me/password", as: "member", body: `{"current":"` + sentinel("current") + `","new":"` + sentinel("new") + `"}`},
		{method: "POST", path: "/api/providers", body: `{"name":"p-` + run + `","type":"openai_compatible","auth_mode":"api_key","base_url":"https://` +
			sentinel("user") + `:` + sentinel("pass") + `@example.com","api_key":"` + sentinel("api-key") + `"}`},
		{method: "PUT", path: "/api/settings/sso", body: `{"issuer_url":"https://idp.example.com","client_id":"cid","client_secret":"` +
			sentinel("client-secret") + `","redirect_url":"https://router.example.com/auth/callback"}`},
		{method: "POST", path: "/api/telemetry/destinations", body: `{"name":"d-` + run + `","kind":"otlp","endpoint":"https://` +
			sentinel("endpoint") + `.example.com","headers":[{"name":"Authorization","value":"` + sentinel("header") +
			`"}],"secrets":{"token":"` + sentinel("secret") + `"}}`},
		{method: "POST", path: "/api/teams", as: "member", body: `{"name":"t-` + run + `"}`},
		{method: "POST", path: "/api/teams", as: "none", body: `{"name":"t-` + run + `"}`},
		{method: "POST", path: "/api/teams", as: "bad-token", body: `{"name":"t-` + run + `"}`},
		{method: "PUT", path: "/api/workspaces/files", as: "member", body: multi.String(), ctype: mw.FormDataContentType()},
		{method: "POST", path: "/api/chats", as: "member", body: `{"title":"` + sentinel("chat") + `"}`},
		{method: "GET", path: "/api/users"},
	} {
		do(c)
	}
	code, out = do(call{method: "POST", path: "/api/keys", as: "member", body: `{"name":"k-` + run + `","user_id":"` +
		memberID.String() + `","tags":["` + sentinel("key-tag") + `"]}`})
	if code != http.StatusCreated {
		t.Fatalf("create key: %d %v", code, out)
	}
	issued = append(issued, out["key"].(string))
	code, out = do(call{method: "POST", path: "/api/gateway-groups/" + groupID + "/rotate-token"})
	if code != http.StatusOK {
		t.Fatalf("rotate: %d %v", code, out)
	}
	issued = append(issued, out["join_token"].(string))
	// Providers and destinations made above are removed through the API too.
	st.Pool.Exec(ctx, `DELETE FROM providers WHERE name=$1`, "p-"+run)
	st.Pool.Exec(ctx, `DELETE FROM telemetry_destinations WHERE name=$1`, "d-"+run)

	type row struct {
		kind, actorID, email, action, ttype, tid, ip string
		status                                       int
		detail                                       string
	}
	rows, err := st.Pool.Query(ctx, `SELECT actor_kind, coalesce(actor_user_id::text,''), coalesce(actor_email,''), action,
		coalesce(target_type,''), coalesce(target_id,''), coalesce(client_ip,''), status, coalesce(detail::text,'')
		FROM audit_events WHERE id > $1 ORDER BY id`, start)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.kind, &x.actorID, &x.email, &x.action, &x.ttype, &x.tid, &x.ip, &x.status, &x.detail)
		return x, err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range got {
		all := x.detail + " " + x.email + " " + x.tid + " " + x.ip + " " + x.action
		if strings.Contains(all, "sentinel-") {
			t.Errorf("%s recorded a secret or personal value: %s", x.action, all)
		}
		for _, s := range issued {
			if strings.Contains(all, s) {
				t.Errorf("%s recorded an issued credential", x.action)
			}
		}
		if x.email != "" && x.email != memberEmail {
			t.Errorf("%s: actor_email %q is not the actor's own", x.action, x.email)
		}
		if x.ip != "192.0.2.7" {
			t.Errorf("%s: client_ip %q", x.action, x.ip)
		}
		switch x.action {
		case "GET /api/users", "POST /api/chats":
			t.Errorf("%s should not be audited", x.action)
		}
	}
	find := func(action, kind string, status int) *row {
		for i := range got {
			if got[i].action == action && got[i].kind == kind && got[i].status == status {
				return &got[i]
			}
		}
		t.Errorf("no %s row for %s by %s", http.StatusText(status), action, kind)
		return nil
	}
	if r := find("POST /api/teams", audit.User, http.StatusForbidden); r != nil && r.actorID != memberID.String() {
		t.Errorf("403 row actor %q", r.actorID)
	}
	if r := find("POST /api/teams", audit.Anonymous, http.StatusUnauthorized); r != nil && r.actorID != "" {
		t.Errorf("401 row has an actor")
	}
	if r := find("POST /api/users", audit.AdminToken, http.StatusCreated); r != nil &&
		(r.ttype != "user" || r.tid != userID || r.detail != `{"role": "member", "rpm_limit": 7}`) {
		t.Errorf("create user row: %+v", *r)
	}
	if r := find("PATCH /api/users/{id}", audit.AdminToken, http.StatusNoContent); r != nil &&
		(r.tid != userID || r.detail != `{"rpm_limit": 9}`) {
		t.Errorf("patch user row: %+v", *r)
	}
	if r := find("POST /api/me/password", audit.User, http.StatusForbidden); r != nil && (r.ttype != "user" || r.tid != memberID.String()) {
		t.Errorf("own password row: %+v", *r)
	}
	if r := find("POST /api/gateway-groups/{id}/rotate-token", audit.AdminToken, http.StatusOK); r != nil &&
		(r.ttype != "gateway_group" || r.tid != groupID) {
		t.Errorf("rotate row: %+v", *r)
	}
	find("PUT /api/workspaces/files", audit.User, http.StatusNotFound)

	// The read API.
	code, _ = do(call{method: "GET", path: "/api/audit", as: "member"})
	if code != http.StatusForbidden {
		t.Errorf("member read the audit log: %d", code)
	}
	for _, bad := range []string{"since=yesterday", "until=2026-13-01T00:00:00Z", "status=maybe", "limit=0", "actor=me", "before=x"} {
		if code, _ := do(call{method: "GET", path: "/api/audit?" + bad}); code != http.StatusBadRequest {
			t.Errorf("?%s: %d", bad, code)
		}
	}
	req := httptest.NewRequest("GET", "/api/audit?status=err&actor="+memberID.String()+"&action="+
		strings.ReplaceAll("POST /api/teams", " ", "%20")+"&since="+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), nil)
	req.Header.Set("Authorization", "Bearer test-admin-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var listed []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if len(listed) != 1 || listed[0]["status"] != float64(403) {
		t.Errorf("filtered list: %v", listed)
	}
	req = httptest.NewRequest("GET", "/api/audit?limit=2", nil)
	req.Header.Set("Authorization", "Bearer test-admin-token")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	json.Unmarshal(w.Body.Bytes(), &listed)
	if len(listed) != 2 || listed[0]["id"].(float64) <= listed[1]["id"].(float64) {
		t.Fatalf("page: %v", listed)
	}
	next := int64(listed[1]["id"].(float64))
	req = httptest.NewRequest("GET", "/api/audit?limit=1&before="+strconv.FormatInt(next, 10), nil)
	req.Header.Set("Authorization", "Bearer test-admin-token")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	json.Unmarshal(w.Body.Bytes(), &listed)
	if len(listed) != 1 || int64(listed[0]["id"].(float64)) >= next {
		t.Errorf("next page: %v", listed)
	}
}

// TestAuditStreamingRoute: the build route streams through a direct
// w.(http.Flusher) assertion; audited must not hide it.
func TestAuditStreamingRoute(t *testing.T) {
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
	m := &Server{Store: st, AdminToken: "test-admin-token", Audit: &audit.Recorder{Pool: st.Pool}}
	mux := http.NewServeMux()
	mux.Handle("POST /api/workspace-templates/{id}/build", m.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		send, ok := stream(w)
		if !ok {
			httpError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		send("one\n")
		send("two\n")
	}))
	req := httptest.NewRequest("POST", "/api/workspace-templates/"+uuid.NewString()+"/build", nil)
	req.Header.Set("Authorization", "Bearer test-admin-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !w.Flushed || w.Body.String() != "one\ntwo\n" {
		t.Errorf("stream: %d flushed=%v %q", w.Code, w.Flushed, w.Body.String())
	}
}

// TestAuditAppendOnly: rows cannot be changed or removed, except in a
// maintenance transaction such as retention's.
func TestAuditAppendOnly(t *testing.T) {
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
	action := "test.append-only." + uuid.NewString()
	insert := func(age string) int64 {
		var id int64
		if err := st.Pool.QueryRow(ctx, `INSERT INTO audit_events (ts, actor_kind, action, status)
			VALUES (now() - $1::interval, 'anonymous', $2, 200) RETURNING id`, age, action).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := insert("0")
	if _, err := st.Pool.Exec(ctx, `UPDATE audit_events SET status=500 WHERE id=$1`, id); err == nil {
		t.Error("UPDATE was allowed")
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM audit_events WHERE id=$1`, id); err == nil {
		t.Error("DELETE was allowed")
	}
	// The setting lasts only for its own transaction.
	if err := audit.Maintain(ctx, st.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE audit_events SET status=201 WHERE id=$1`, id)
		return err
	}); err != nil {
		t.Errorf("UPDATE under maintenance: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM audit_events WHERE id=$1`, id); err == nil {
		t.Error("DELETE was allowed after a maintenance transaction")
	}

	rec := &audit.Recorder{Pool: st.Pool}
	old := insert("1300 months")
	if err := rec.Prune(ctx, 1200); err != nil {
		t.Fatal(err)
	}
	var left []int64
	rows, _ := st.Pool.Query(ctx, `SELECT id FROM audit_events WHERE action=$1 ORDER BY id`, action)
	left, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != id {
		t.Errorf("after pruning: %v (old row %d, recent row %d)", left, old, id)
	}
	audit.Maintain(ctx, st.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE action=$1`, action)
		return err
	})
}
