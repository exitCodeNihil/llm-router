package mgmt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/store"
	"github.com/exitcodenihil/llm-router/internal/telemetry"
	"github.com/exitcodenihil/llm-router/internal/telemetry/langfuse"
	"github.com/exitcodenihil/llm-router/internal/telemetry/otlp"
)

// TestDestinationsAPIAgainstPostgres needs an empty, disposable database:
//
//	LLMR_TEST_DATABASE_URL=postgres://... go test ./internal/mgmt
func TestDestinationsAPIAgainstPostgres(t *testing.T) {
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
	st.Pool.Exec(ctx, `DELETE FROM telemetry_destinations`)
	telemetry.Register(otlp.Kind{}, langfuse.Kind{})
	const key = "test-encryption-key-0123456789"
	holder := &snapshot.Holder{}
	holder.Set(&snapshot.Snapshot{})
	m := &Server{Store: st, AdminToken: "test-admin-token", EncryptionKey: key, Snapshots: holder}
	mux := http.NewServeMux()
	m.Register(mux)
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-admin-token")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	stored := func(id string) *snapshot.Destination {
		t.Helper()
		d, err := m.loadDestination(ctx, uuid.MustParse(id))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	code, body := call("GET", "/api/telemetry/kinds", "")
	if code != 200 || body != `[{"beta":false,"label":"Langfuse","name":"langfuse","secret_fields":["secret_key"]},`+
		`{"beta":true,"label":"OpenTelemetry (OTLP/HTTP)","name":"otlp","secret_fields":[]}]`+"\n" {
		t.Fatalf("kinds: %d %s", code, body)
	}

	// Scope values name teams and gateway groups that exist.
	teamX, otherTeam, g1 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	st.Pool.Exec(ctx, `INSERT INTO teams (id, name) VALUES ($1, $2), ($3, $4)`, teamX, "x-"+teamX, otherTeam, "o-"+otherTeam)
	st.Pool.Exec(ctx, `INSERT INTO gateway_groups (id, name, join_token_hash) VALUES ($1, $2, $3)`, g1, "g-"+g1, []byte(g1))
	missing := uuid.NewString()
	for _, c := range []struct{ scopes, want string }{
		{`"team_scope":{"mode":"selected","values":["` + missing + `"]}`, "unknown team " + missing},
		{`"team_scope":{"mode":"except","values":["team-x"]}`, "unknown team team-x"},
		{`"group_scope":{"mode":"selected","values":["_control_plane","` + missing + `"]}`, "unknown gateway group " + missing},
		{`"group_scope":{"mode":"selected","values":["` + teamX + `"]}`, "unknown gateway group " + teamX},
	} {
		code, body := call("POST", "/api/telemetry/destinations", `{"name":"s","kind":"otlp","endpoint":"http://x",`+c.scopes+`}`)
		if code != 400 || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %s", c.scopes, code, body)
		}
	}

	// Create: secrets and header values go in, and never come out.
	code, body = call("POST", "/api/telemetry/destinations", `{"name":"Tempo","kind":"otlp","endpoint":"http://tempo:4318",
		"config":{"trace_link":"http://grafana/explore?trace={trace_id}"},
		"headers":[{"name":"Authorization","value":"Bearer t0ken"},{"name":"X-Scope-OrgID","value":"acme"}],
		"team_scope":{"mode":"except","values":["`+teamX+`","`+teamX+`"]},"group_scope":{"mode":"selected","values":["_control_plane","`+g1+`"]}}`)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var created struct{ ID string }
	json.Unmarshal([]byte(body), &created)
	code, body = call("GET", "/api/telemetry/destinations", "")
	if code != 200 || strings.Contains(body, "t0ken") || strings.Contains(body, "acme") {
		t.Fatalf("list leaks a secret: %d %s", code, body)
	}
	var list []destinationView
	json.Unmarshal([]byte(body), &list)
	v := list[0]
	if v.ID != created.ID || v.Kind != "otlp" || !v.Enabled || v.ContentMode != "metadata_only" || v.MaxContentBytes != 65536 ||
		strings.Join(v.Headers, ",") != "Authorization,X-Scope-OrgID" || len(v.SecretNames) != 0 ||
		v.TeamScope.Mode != "except" || strings.Join(v.TeamScope.Values, ",") != teamX ||
		strings.Join(v.GroupScope.Values, ",") != "_control_plane,"+g1 || v.Health.Status != "no_data" ||
		v.Config["trace_link"] != "http://grafana/explore?trace={trace_id}" {
		t.Fatalf("view %+v", v)
	}
	var enc []byte
	st.Pool.QueryRow(ctx, `SELECT secrets_enc FROM telemetry_destinations WHERE id=$1`, created.ID).Scan(&enc)
	if strings.Contains(string(enc), "t0ken") {
		t.Fatal("secret stored in plaintext")
	}

	// Update: a header listed without a value keeps it; one not listed goes.
	code, body = call("PUT", "/api/telemetry/destinations/"+created.ID, `{"name":"Tempo","kind":"otlp","endpoint":"http://tempo:4318",
		"headers":[{"name":"Authorization"}],"content_mode":"content","max_content_bytes":1000}`)
	if code != 204 {
		t.Fatalf("put: %d %s", code, body)
	}
	if d := stored(created.ID); d.Secrets["header:Authorization"] != "Bearer t0ken" || len(d.Secrets) != 1 ||
		d.ContentMode != "content" || d.MaxContentBytes != 1000 || d.Teams.Mode != "all" || len(d.Config) != 0 {
		t.Fatalf("after put %+v", d)
	}
	// Headers left out entirely keep every stored header.
	if code, body = call("PUT", "/api/telemetry/destinations/"+created.ID, `{"name":"Tempo","kind":"otlp","endpoint":"http://tempo:4318","enabled":false}`); code != 204 {
		t.Fatalf("put: %d %s", code, body)
	}
	if d := stored(created.ID); d.Secrets["header:Authorization"] != "Bearer t0ken" || d.Enabled {
		t.Fatalf("after put %+v", d)
	}

	// Langfuse: the secret key is required, then kept when left out.
	if code, body = call("POST", "/api/telemetry/destinations", `{"name":"LF","kind":"langfuse","endpoint":"https://cloud.langfuse.com",
		"config":{"public_key":"pk-lf"}}`); code != 400 || !strings.Contains(body, "secret key") {
		t.Fatalf("langfuse without a secret: %d %s", code, body)
	}
	code, body = call("POST", "/api/telemetry/destinations", `{"name":"LF","kind":"langfuse","endpoint":"https://cloud.langfuse.com",
		"config":{"public_key":"pk-lf","ui_url":"https://langfuse.example.com"},"secrets":{"secret_key":"sk-lf"}}`)
	if code != 201 {
		t.Fatalf("langfuse: %d %s", code, body)
	}
	json.Unmarshal([]byte(body), &created)
	lfID := created.ID
	if code, body = call("PUT", "/api/telemetry/destinations/"+lfID, `{"name":"LF","kind":"langfuse","endpoint":"https://cloud.langfuse.com",
		"config":{"public_key":"pk-lf2"}}`); code != 204 {
		t.Fatalf("put: %d %s", code, body)
	}
	if d := stored(lfID); d.Secrets["secret_key"] != "sk-lf" || d.Config["public_key"] != "pk-lf2" {
		t.Fatalf("after put %+v", d)
	}
	code, body = call("GET", "/api/telemetry/destinations/"+lfID, "")
	if code != 200 || !strings.Contains(body, `"secret_names":["secret_key"]`) || strings.Contains(body, "sk-lf") {
		t.Fatalf("get: %d %s", code, body)
	}

	// Refusals.
	for _, c := range []struct{ body, want string }{
		{`{"name":"LF","kind":"otlp","endpoint":"http://x"}`, "already exists"},
		{`{"name":"","kind":"otlp","endpoint":"http://x"}`, "name is required"},
		{`{"name":"a","kind":"nope"}`, "unknown kind"},
		{`{"name":"a","kind":"otlp"}`, "endpoint is required"},
		{`{"name":"a","kind":"otlp","endpoint":"http://x","content_mode":"all"}`, "content_mode"},
		{`{"name":"a","kind":"otlp","endpoint":"http://x","max_content_bytes":2000000}`, "max_content_bytes"},
		{`{"name":"a","kind":"otlp","endpoint":"http://x","team_scope":{"mode":"some"}}`, "scope mode"},
		{`{"name":"a","kind":"otlp","endpoint":"http://x","secrets":{"secret_key":"x"}}`, "unknown secret"},
		{`{"name":"a","kind":"otlp","endpoint":"http://x","headers":[{"name":"X-New"}]}`, "needs a value"},
	} {
		code, body := call("POST", "/api/telemetry/destinations", c.body)
		if code < 400 || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %s", c.body, code, body)
		}
	}
	if code, _ := call("PUT", "/api/telemetry/destinations/"+uuid.NewString(), `{"name":"z","kind":"otlp","endpoint":"http://x"}`); code != 404 {
		t.Errorf("put unknown: %d", code)
	}

	// Test: a draft, a saved destination, and a draft that borrows the saved
	// secrets.
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if auth != "Bearer t0ken" {
			w.WriteHeader(401)
		}
	}))
	defer srv.Close()
	code, body = call("POST", "/api/telemetry/destinations/test", `{"name":"draft","kind":"otlp","endpoint":"`+srv.URL+`",
		"headers":[{"name":"Authorization","value":"Bearer t0ken"}],"config":{"trace_link":"http://ui/{trace_id}"}}`)
	var res telemetry.TestResult
	json.Unmarshal([]byte(body), &res)
	if code != 200 || !res.OK || !strings.HasPrefix(res.TraceURL, "http://ui/") || len(res.TraceURL) != len("http://ui/")+32 {
		t.Fatalf("test draft: %d %s", code, body)
	}
	// A new host doesn't get the stored header: it must be entered again,
	// for a test as for a save.
	auth = "unset"
	code, body = call("POST", "/api/telemetry/destinations/test", `{"id":"`+list[0].ID+`","name":"Tempo","kind":"otlp","endpoint":"`+srv.URL+`"}`)
	if json.Unmarshal([]byte(body), &res); res.OK || !strings.Contains(res.Detail, "the endpoint changed") || auth != "unset" {
		t.Fatalf("test on a new host with the stored header: %d %s", code, body)
	}
	if code, body = call("PUT", "/api/telemetry/destinations/"+list[0].ID, `{"name":"Tempo","kind":"otlp","endpoint":"`+srv.URL+`"}`); code != 400 ||
		!strings.Contains(body, "the endpoint changed: enter the secret key and header values again") {
		t.Fatalf("put on a new host with the stored header: %d %s", code, body)
	}
	if code, body = call("PUT", "/api/telemetry/destinations/"+list[0].ID, `{"name":"Tempo","kind":"otlp","endpoint":"`+srv.URL+`",
		"headers":[{"name":"Authorization","value":"Bearer t0ken"}]}`); code != 204 {
		t.Fatalf("put on a new host: %d %s", code, body)
	}
	code, body = call("POST", "/api/telemetry/destinations/test", `{"id":"`+list[0].ID+`"}`)
	if json.Unmarshal([]byte(body), &res); !res.OK || auth != "Bearer t0ken" {
		t.Fatalf("test saved: %d %s (auth %q)", code, body, auth)
	}
	code, body = call("POST", "/api/telemetry/destinations/test", `{"id":"`+list[0].ID+`","name":"Tempo","kind":"otlp",
		"endpoint":"`+srv.URL+`","headers":[{"name":"Authorization","value":"Bearer wrong"}]}`)
	if json.Unmarshal([]byte(body), &res); res.OK || res.Status != 401 {
		t.Fatalf("test with a wrong header: %d %s", code, body)
	}

	// Secrets that can't be decrypted must be entered again.
	m.EncryptionKey = "another-key"
	if code, body = call("PUT", "/api/telemetry/destinations/"+lfID, `{"name":"LF","kind":"langfuse","endpoint":"https://cloud.langfuse.com",
		"config":{"public_key":"pk-lf2"}}`); code != 400 || !strings.Contains(body, "decrypted") {
		t.Fatalf("put over undecryptable secrets: %d %s", code, body)
	}
	if code, body = call("PUT", "/api/telemetry/destinations/"+lfID, `{"name":"LF","kind":"langfuse","endpoint":"https://cloud.langfuse.com",
		"config":{"public_key":"pk-lf2"},"secrets":{"secret_key":"sk-new"}}`); code != 204 {
		t.Fatalf("re-entered secrets: %d %s", code, body)
	}
	m.EncryptionKey = key

	// Deleting a team or a gateway group takes it out of every scope.
	teamID, groupID := uuid.New(), uuid.New()
	st.Pool.Exec(ctx, `INSERT INTO teams (id, name) VALUES ($1, $2)`, teamID, "scoped-"+teamID.String())
	st.Pool.Exec(ctx, `INSERT INTO gateway_groups (id, name, join_token_hash) VALUES ($1, $2, $3)`, groupID, "g-"+groupID.String(), groupID[:])
	// Back on the original key, so the secret is entered once more.
	if code, body := call("PUT", "/api/telemetry/destinations/"+lfID, `{"name":"LF","kind":"langfuse","endpoint":"https://cloud.langfuse.com",
		"config":{"public_key":"pk"},"secrets":{"secret_key":"sk-new"},"team_scope":{"mode":"selected","values":["`+teamID.String()+`","`+otherTeam+`"]},
		"group_scope":{"mode":"except","values":["`+groupID.String()+`"]}}`); code != 204 {
		t.Fatalf("put scopes: %d %s", code, body)
	}
	if code, body := call("DELETE", "/api/teams/"+teamID.String(), ""); code != 204 {
		t.Fatalf("delete team: %d %s", code, body)
	}
	if code, body := call("DELETE", "/api/gateway-groups/"+groupID.String(), ""); code != 204 {
		t.Fatalf("delete group: %d %s", code, body)
	}
	if d := stored(lfID); strings.Join(d.Teams.Values, ",") != otherTeam || len(d.Groups.Values) != 0 {
		t.Fatalf("scopes after deletes: %+v %+v", d.Teams, d.Groups)
	}

	// The snapshot reads what the API wrote.
	snap, err := (&snapshot.Builder{Pool: st.Pool, EncryptionKey: key}).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Destinations) != 2 || snap.Destinations[0].Name != "LF" || snap.Destinations[0].Secrets["secret_key"] != "sk-new" {
		t.Fatalf("snapshot destinations %+v", snap.Destinations)
	}

	if code, _ := call("DELETE", "/api/telemetry/destinations/"+lfID, ""); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := call("GET", "/api/telemetry/destinations/"+lfID, ""); code != 404 {
		t.Fatalf("get deleted: %d", code)
	}

	// The policy endpoint is policy only.
	if code, body := call("PUT", "/api/settings/telemetry", `{"mode":"by_rule","rules":[{"scope_type":"team","scope_value":"t","capture":"meta","sample":10,"enabled":true}]}`); code != 204 {
		t.Fatalf("put policy: %d %s", code, body)
	}
	code, body = call("GET", "/api/settings/telemetry", "")
	var policy snapshot.Telemetry
	json.Unmarshal([]byte(body), &policy)
	if code != 200 || policy.Mode != "by_rule" || len(policy.Rules) != 1 || policy.Rules[0].ID == "" {
		t.Fatalf("get policy: %d %s", code, body)
	}
	for _, bad := range []string{`{"mode":"some"}`, `{"rules":[{"scope_type":"org","capture":"meta"}]}`,
		`{"rules":[{"scope_type":"team","capture":"all"}]}`, `{"rules":[{"scope_type":"team","capture":"meta","sample":101}]}`} {
		if code, _ := call("PUT", "/api/settings/telemetry", bad); code != 400 {
			t.Errorf("policy %s: %d", bad, code)
		}
	}
	st.Pool.Exec(ctx, `DELETE FROM telemetry_destinations`)
	st.Pool.Exec(ctx, `DELETE FROM settings WHERE key='telemetry'`)
}
