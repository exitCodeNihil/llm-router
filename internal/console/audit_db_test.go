package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/audit"
	"github.com/exitcodenihil/llm-router/internal/store"
)

// TestSignInAudit: sign-ins are recorded with their user, failures with no
// trace of the address that was tried.
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/console
func TestSignInAudit(t *testing.T) {
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
	id := uuid.New()
	email := "signin-" + id.String()[:8] + "@example.com"
	hash, _ := HashPassword("the-right-password")
	if _, err := st.Pool.Exec(ctx, `INSERT INTO users (id, email, name, role, password_hash) VALUES ($1,$2,'s','member',$3)`,
		id, email, hash); err != nil {
		t.Fatal(err)
	}
	defer st.Pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, id)

	var events []audit.Event
	a := &Auth{Store: st, EncryptionKey: "test-encryption-key-0123456789",
		Audit: func(_ context.Context, e audit.Event) { events = append(events, e) }}
	mux := http.NewServeMux()
	a.Register(mux)
	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.RemoteAddr = "198.51.100.4:1000"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	typo := "sentinel-typo-" + id.String()[:8] + "@example.com"
	post("/auth/password", `{"email":"`+typo+`","password":"x"}`)
	post("/auth/password", `{"email":"`+email+`","password":"wrong-password"}`)
	ok := post("/auth/password", `{"email":"`+email+`","password":"the-right-password"}`)
	post("/auth/setup", `{"email":"`+typo+`","password":"long-enough-password"}`)

	req := httptest.NewRequest("GET", "/auth/logout", nil)
	req.RemoteAddr = "198.51.100.4:1000"
	for _, c := range ok.Result().Cookies() {
		req.AddCookie(c)
	}
	mux.ServeHTTP(httptest.NewRecorder(), req)
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/auth/logout", nil))

	want := []struct {
		action string
		status int
		user   string
	}{
		{"auth.signin_failed", http.StatusUnauthorized, ""},
		{"auth.signin_failed", http.StatusUnauthorized, ""},
		{"auth.signin", http.StatusNoContent, id.String()},
		{"auth.setup", http.StatusForbidden, ""},
		{"auth.signout", http.StatusFound, id.String()},
	}
	if len(events) != len(want) {
		t.Fatalf("%d events, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		e := events[i]
		if e.Action != w.action || e.Status != w.status || e.ActorUserID != w.user || e.ClientIP != "198.51.100.4" {
			t.Errorf("event %d: %+v, want %+v", i, e, w)
		}
		if w.user == "" && (e.ActorKind != audit.Anonymous || e.ActorEmail != "") {
			t.Errorf("event %d names an actor: %+v", i, e)
		}
		if w.user != "" && (e.ActorKind != audit.User || e.ActorEmail != email) {
			t.Errorf("event %d: actor %+v", i, e)
		}
		if strings.Contains(e.ActorEmail+e.TargetID+string(e.Detail), "sentinel-") {
			t.Errorf("event %d kept the address that was tried", i)
		}
	}
}
