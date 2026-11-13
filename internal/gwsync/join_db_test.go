package gwsync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/limits"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
)

// TestJoinAndRenewRefusals: a join or renew is answered 401, marked as the
// control plane's own verdict, only when the token, group or instance does
// not exist. A database that cannot be asked is a 503, which a gateway
// retries without counting it as a refusal.
func TestJoinAndRenewRefusals(t *testing.T) {
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
	groupID := uuid.NewString()
	token := JoinTokenPrefix + "join-" + groupID
	hash := sha256.Sum256([]byte(token))
	if _, err := st.Pool.Exec(ctx, `INSERT INTO gateway_groups (id, name, join_token_hash) VALUES ($1,$2,$3)`,
		groupID, "join-"+groupID, hash[:]); err != nil {
		t.Fatal(err)
	}
	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{Version: 1, Spend: map[string]snapshot.Spend{},
		GatewayGroups: map[string]*snapshot.GatewayGroup{groupID: {ID: groupID, Name: "join", DataRule: "inherit"}}})
	key := auth.DeriveKey("llmr-gw-session", "test")
	serve := func(st *store.Store) *httptest.Server {
		mux := http.NewServeMux()
		(&Server{Store: st, Snapshots: holder, Limits: limits.New(), Key: key}).Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	}
	srv := serve(st)
	// The same control plane with its database gone.
	down, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	down.Close()
	broken := serve(down)

	// post returns the status, whether the answer is marked as a refusal, and the body.
	post := func(srv *httptest.Server, path, bearer string) (int, bool, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(`{"name":"gw","version":"0.3.1","schema":2}`))
		req.Header.Set("Authorization", "Bearer "+bearer)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header.Get("X-Llmr-Refused") == "1", string(body)
	}
	expect := func(what string, wantCode int, wantRefused bool) func(int, bool, string) {
		return func(code int, refused bool, body string) {
			t.Helper()
			if code != wantCode || refused != wantRefused {
				t.Fatalf("%s: %d refused=%v %s; want %d refused=%v", what, code, refused, body, wantCode, wantRefused)
			}
		}
	}

	code, refused, body := post(srv, "/gw/v1/join", token)
	expect("join", 200, false)(code, refused, body)
	var joined sessionResponse
	if err := json.Unmarshal([]byte(body), &joined); err != nil || joined.Session == "" {
		t.Fatalf("join answered %s", body)
	}
	expect("renew", 200, false)(post(srv, "/gw/v1/renew", joined.Session))

	expect("join with an unknown token", 401, true)(post(srv, "/gw/v1/join", JoinTokenPrefix+"nobody-"+groupID))
	expect("join with something that is no join token", 401, true)(post(srv, "/gw/v1/join", "llmr_a-gateway-key"))

	expect("join while the database is down", 503, false)(post(broken, "/gw/v1/join", token))
	expect("join with an unknown token while the database is down", 503, false)(
		post(broken, "/gw/v1/join", JoinTokenPrefix+"nobody-"+groupID))
	expect("renew while the database is down", 503, false)(post(broken, "/gw/v1/renew", joined.Session))
	// An expired or forged session is not the verdict on the gateway: it joins again.
	expect("renew with a bad session", 401, false)(post(srv, "/gw/v1/renew", "not-a-session"))

	if _, err := st.Pool.Exec(ctx, `DELETE FROM gateway_instances WHERE id=$1`, joined.InstanceID); err != nil {
		t.Fatal(err)
	}
	expect("renew for a removed instance", 401, true)(post(srv, "/gw/v1/renew", joined.Session))
	expect("join again after the instance was removed", 200, false)(post(srv, "/gw/v1/join", token))

	if _, err := st.Pool.Exec(ctx, `DELETE FROM gateway_groups WHERE id=$1`, groupID); err != nil {
		t.Fatal(err)
	}
	expect("join after the group was deleted", 401, true)(post(srv, "/gw/v1/join", token))
	expect("renew after the group was deleted", 401, true)(post(srv, "/gw/v1/renew", joined.Session))
}
