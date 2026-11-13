package mgmt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/exitcodenihil/llm-router/internal/audit"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// TestDeleteAndEraseAgainstPostgres: a deleted user's email and key names
// keep labelling their usage until the user is erased, and then appear
// nowhere.
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/mgmt
func TestDeleteAndEraseAgainstPostgres(t *testing.T) {
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
	if err := usage.NewPGWriter(st.Pool).EnsurePartitions(ctx); err != nil {
		t.Fatal(err)
	}
	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{})
	m := &Server{Store: st, AdminToken: "test-admin-token", EncryptionKey: "test-encryption-key-0123456789",
		Snapshots: holder, Audit: &audit.Recorder{Pool: st.Pool}}
	mux := http.NewServeMux()
	m.Register(mux)
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-admin-token")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	create := func(path, body string) string {
		t.Helper()
		code, out := call("POST", path, body)
		var v struct{ ID string }
		if json.Unmarshal([]byte(out), &v); code != http.StatusCreated || v.ID == "" {
			t.Fatalf("POST %s: %d %s", path, code, out)
		}
		return v.ID
	}
	run := uuid.NewString()[:8]
	email := "Erase_Me%" + run + "@example.com"
	userID := create("/api/users", `{"email":"`+email+`","role":"member"}`)
	keyID := create("/api/keys", `{"name":"key-of-`+run+`","user_id":"`+userID+`"}`)
	teamID := create("/api/teams", `{"name":"team-`+run+`"}`)
	if code, out := call("POST", "/api/teams/"+teamID+"/members", `{"user_id":"`+email+`"}`); code != http.StatusNoContent {
		t.Fatalf("add member: %d %s", code, out)
	}
	call("PATCH", "/api/users/"+userID, `{"rpm_limit":3}`)
	defer st.Pool.Exec(ctx, `DELETE FROM teams WHERE id=$1`, teamID)

	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := usage.InsertEvents(ctx, tx, []usage.Event{{TS: time.Now().UTC(), RequestID: uuid.NewString(),
		APIKeyID: keyID, UserID: userID, ModelName: "m-" + run, CostUSD: 0.5, StatusCode: 200, Attempts: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	breakdown := func(dim string) string {
		t.Helper()
		code, out := call("GET", "/api/usage/breakdown?group_by="+dim+"&model=m-"+run, "")
		if code != http.StatusOK {
			t.Fatalf("breakdown: %d %s", code, out)
		}
		var rows []struct{ Label string }
		json.Unmarshal([]byte(out), &rows)
		if len(rows) != 1 {
			t.Fatalf("breakdown by %s: %s", dim, out)
		}
		return rows[0].Label
	}

	if code, out := call("DELETE", "/api/users/"+userID, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, out)
	}
	if got := breakdown("user"); got != email+" (deleted)" {
		t.Errorf("user label after delete: %q", got)
	}
	if got := breakdown("key"); got != "key-of-"+run+" (deleted)" {
		t.Errorf("key label after delete: %q", got)
	}
	_, out := call("GET", "/api/deleted-users", "")
	if !strings.Contains(out, `"label":"`+email+`"`) {
		t.Errorf("deleted users: %s", out)
	}
	// The address is free again.
	again := create("/api/users", `{"email":"`+email+`","role":"member"}`)
	if code, out := call("DELETE", "/api/users/"+again, ""); code != http.StatusNoContent {
		t.Fatalf("delete again: %d %s", code, out)
	}

	// Details that name the address (none of today's policies keep one), and
	// one that only an unescaped LIKE pattern would match.
	var named, lookalike int64
	insert := func(detail string) (id int64) {
		t.Helper()
		if err := st.Pool.QueryRow(ctx, `INSERT INTO audit_events (actor_kind, action, status, detail)
			VALUES ('admin_token', 'test.erase', 200, $1::jsonb) RETURNING id`, detail).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	named = insert(`{"note":"` + strings.ToUpper(email) + `"}`)
	lookalike = insert(`{"note":"EraseXMeYY` + run + `@example.com"}`)

	code, out := call("POST", "/api/users/"+userID+"/erase", "")
	if code != http.StatusOK || !strings.Contains(out, `"erased":true`) || !strings.Contains(out, "database backups") {
		t.Fatalf("erase: %d %s", code, out)
	}
	if got := breakdown("user"); got != "(erased)" {
		t.Errorf("user label after erase: %q", got)
	}
	if got := breakdown("key"); got != "(erased)" {
		t.Errorf("key label after erase: %q", got)
	}
	// A live user erased in one step.
	if code, out := call("POST", "/api/users/"+again+"/erase", ""); code != http.StatusOK {
		t.Fatalf("erase the second: %d %s", code, out)
	}
	var n int
	st.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM users WHERE email ILIKE $1) +
		(SELECT count(*) FROM deleted_entities WHERE label ILIKE $1 OR label LIKE 'key-of-' || $2) +
		(SELECT count(*) FROM audit_events WHERE actor_email ILIKE $1 OR detail::text ILIKE '%key-of-' || $2 || '%'
			OR detail::text ILIKE '%Erase\_Me\%' || $2 || '%'
			OR (actor_user_id = $3 AND actor_email IS NOT NULL)
			OR (target_type = 'user' AND target_id = $3::text AND detail IS NOT NULL))`,
		email, run, userID).Scan(&n)
	if n != 0 {
		t.Errorf("%d rows still hold the erased user's data", n)
	}

	var namedGone, lookalikeKept bool
	st.Pool.QueryRow(ctx, `SELECT (SELECT detail IS NULL FROM audit_events WHERE id=$1),
		(SELECT detail IS NOT NULL FROM audit_events WHERE id=$2)`, named, lookalike).Scan(&namedGone, &lookalikeKept)
	if !namedGone || !lookalikeKept {
		t.Errorf("detail naming the address cleared: %v; look-alike kept: %v", namedGone, lookalikeKept)
	}

	if code, _ := call("POST", "/api/users/"+uuid.NewString()+"/erase", ""); code != http.StatusNotFound {
		t.Errorf("erase of an unknown id: %d", code)
	}
}

// TestEraseLastAdmin: the only admin cannot be erased. Other tests' admins
// share the database, so they are disabled for the call and then restored.
func TestEraseLastAdmin(t *testing.T) {
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
	rows, _ := st.Pool.Query(ctx, `UPDATE users SET disabled=true WHERE role='admin' AND NOT disabled RETURNING id`)
	others, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	defer st.Pool.Exec(ctx, `UPDATE users SET disabled=false WHERE id = ANY($1)`, others)
	id := uuid.New()
	if _, err := st.Pool.Exec(ctx, `INSERT INTO users (id, email, role) VALUES ($1, $2, 'admin')`,
		id, "only-"+id.String()+"@example.com"); err != nil {
		t.Fatal(err)
	}
	defer st.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, id)
	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{})
	m := &Server{Store: st, AdminToken: "test-admin-token", EncryptionKey: "test-encryption-key-0123456789", Snapshots: holder}
	mux := http.NewServeMux()
	m.Register(mux)
	for _, method := range []string{"POST /erase", "DELETE "} {
		verb, suffix, _ := strings.Cut(method, " ")
		req := httptest.NewRequest(verb, "/api/users/"+id.String()+suffix, nil)
		req.Header.Set("Authorization", "Bearer test-admin-token")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusConflict {
			t.Errorf("%s the last admin: %d %s", verb, w.Code, w.Body.String())
		}
	}
}

// TestSSOSecretAgainstPostgres: the secret is stored encrypted, and one the
// current key cannot decrypt is reported rather than hidden.
func TestSSOSecretAgainstPostgres(t *testing.T) {
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
	var old []byte
	st.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='sso'`).Scan(&old)
	defer func() {
		if old == nil {
			st.Pool.Exec(ctx, `DELETE FROM settings WHERE key='sso'`)
		} else {
			st.Pool.Exec(ctx, `UPDATE settings SET value=$1 WHERE key='sso'`, old)
		}
	}()
	call := func(key, method, body string) (int, string) {
		m := &Server{Store: st, AdminToken: "test-admin-token", EncryptionKey: key}
		mux := http.NewServeMux()
		m.Register(mux)
		req := httptest.NewRequest(method, "/api/settings/sso", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-admin-token")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	const key = "test-encryption-key-0123456789"
	if code, out := call(key, "PUT", `{"issuer_url":"https://idp.example.com","client_id":"cid",
		"client_secret":"sso-secret-9","redirect_url":"https://r.example.com/auth/callback"}`); code != http.StatusNoContent {
		t.Fatalf("put: %d %s", code, out)
	}
	var raw string
	st.Pool.QueryRow(ctx, `SELECT value::text FROM settings WHERE key='sso'`).Scan(&raw)
	if strings.Contains(raw, "sso-secret-9") {
		t.Fatalf("stored in plaintext: %s", raw)
	}
	if _, out := call(key, "GET", ""); strings.Contains(out, "secret_error") || strings.Contains(out, "sso-secret-9") {
		t.Errorf("get: %s", out)
	}
	if _, out := call("another-encryption-key-0123", "GET", ""); !strings.Contains(out, "cannot be decrypted") ||
		!strings.Contains(out, `"client_id":"cid"`) {
		t.Errorf("get with another key: %s", out)
	}
}
