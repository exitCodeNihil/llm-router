package gwsync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
	"github.com/exitcodenihil/llm-router/internal/usage"
)

// fixture: team A (only "gpt-a") and team B; alice is in A, bob in B, carol in
// no team. Three providers, one per model, plus a second gpt-a deployment in
// another region.
func fixture() *snapshot.Snapshot {
	budget := 5.0
	share := 1.0
	pa := &snapshot.Provider{ID: "pa", Name: "a", Type: "openai_compatible", APIKey: "secret-a"}
	pb := &snapshot.Provider{ID: "pb", Name: "b", Type: "openai_compatible", APIKey: "secret-b"}
	pe := &snapshot.Provider{ID: "pe", Name: "eu", Type: "openai_compatible", APIKey: "secret-eu"}
	teamA := &snapshot.Team{ID: "A", AllowedModels: map[string]bool{"gpt-a": true}}
	teamB := &snapshot.Team{ID: "B", BudgetUSD: &budget}
	alice := &snapshot.User{ID: "alice", Email: "alice@example.com", TeamIDs: []string{"A"}, TeamModels: map[string]bool{"gpt-a": true},
		Memberships: map[string]*snapshot.Membership{"A": {BudgetUSD: &share}}}
	bob := &snapshot.User{ID: "bob", Email: "bob@example.com", TeamIDs: []string{"B"}}
	carol := &snapshot.User{ID: "carol", Email: "carol@example.com"}
	exp := time.Unix(2000000000, 0)
	return &snapshot.Snapshot{
		Version: 7,
		KeysByHash: map[[32]byte]*snapshot.Key{
			{1}: {ID: "k-alice-team", UserID: "alice", TeamID: "A"},
			{2}: {ID: "k-alice-own", UserID: "alice", ExpiresAt: &exp},
			{3}: {ID: "k-bob-team", UserID: "bob", TeamID: "B", BudgetUSD: &budget},
			{4}: {ID: "k-carol", UserID: "carol", AllowedModels: map[string]bool{}},
		},
		UsersByID: map[string]*snapshot.User{"alice": alice, "bob": bob, "carol": carol},
		TeamsByID: map[string]*snapshot.Team{"A": teamA, "B": teamB},
		DeploymentsByModel: map[string][]*snapshot.Deployment{
			"gpt-a": {
				{ID: "d-a-us", Provider: pa, ModelName: "gpt-a", UpstreamName: "a", Region: "us"},
				{ID: "d-a-eu", Provider: pe, ModelName: "gpt-a", UpstreamName: "a", Region: "eu"},
			},
			"gpt-b": {{ID: "d-b", Provider: pb, ModelName: "gpt-b", UpstreamName: "b"}},
		},
		Prices: map[string]snapshot.Price{"gpt-a": {InputPer1M: 1, OutputPer1M: 2}},
		Spend:  map[string]snapshot.Spend{"team:B": {USD: 3}},
		Destinations: []*snapshot.Destination{{Name: "lf", Kind: "langfuse", Enabled: true,
			Secrets: map[string]string{"secret_key": "lf-secret"}}},
	}
}

func keyIDs(c Config) []string {
	var out []string
	for _, k := range c.Keys {
		out = append(out, k.ID)
	}
	return out
}

func TestConfigForAllTeams(t *testing.T) {
	b := buildConfig(fixture(), &snapshot.GatewayGroup{ID: "g", Name: "all"}, schemaVersion)
	if got := strings.Join(keyIDs(b.cfg), ","); got != "k-alice-own,k-alice-team,k-bob-team,k-carol" {
		t.Fatalf("keys = %s", got)
	}
	if len(b.cfg.Deployments) != 3 || len(b.cfg.Providers) != 3 {
		t.Fatalf("an unrestricted group serves every deployment: %+v", b.cfg.Deployments)
	}
	raw, _ := json.Marshal(b.cfg)
	for _, leak := range []string{"lf-secret", "alice@example.com", `"spend"`} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("config carries %s", leak)
		}
	}
	// nil means no restriction, empty means nothing: the two must stay apart.
	if !strings.Contains(string(raw), `"id":"k-carol","user_id":"carol","allowed_models":[]`) ||
		!strings.Contains(string(raw), `"id":"k-alice-own","user_id":"alice","allowed_models":null`) {
		t.Errorf("model lists lost their meaning: %s", raw)
	}
}

// A team's gateway gets its team's keys and its members' personal keys, and
// only the deployments those callers can reach: no other team's credentials.
func TestConfigForOneTeam(t *testing.T) {
	g := &snapshot.GatewayGroup{ID: "g", Name: "team-a", AllowedTeams: map[string]bool{"A": true}}
	b := buildConfig(fixture(), g, schemaVersion)
	if got := strings.Join(keyIDs(b.cfg), ","); got != "k-alice-own,k-alice-team" {
		t.Fatalf("keys = %s", got)
	}
	if len(b.cfg.Users) != 1 || b.cfg.Users[0].ID != "alice" || b.cfg.Users[0].Memberships["A"] == nil {
		t.Fatalf("users = %+v", b.cfg.Users)
	}
	if len(b.cfg.Teams) != 1 || b.cfg.Teams[0].ID != "A" {
		t.Fatalf("teams = %+v", b.cfg.Teams)
	}
	for _, d := range b.cfg.Deployments {
		if d.ModelName == "gpt-b" {
			t.Fatal("team A can't reach gpt-b, so its gateway must not get it")
		}
	}
	for _, p := range b.cfg.Providers {
		if p.APIKey == "secret-b" {
			t.Fatal("team A's gateway got another provider's secret")
		}
	}
}

// Vertex and Entra providers need schema 2: a 0.2.x gateway can't mint their
// tokens, so it must get neither their deployments nor the service account key.
func TestConfigCloudProvidersNeedSchema2(t *testing.T) {
	s := fixture()
	vertex := &snapshot.Provider{ID: "pv", Name: "vertex", Type: "gcp_vertex", AuthMode: "gcp_sa",
		APIKey: `{"type":"service_account","private_key":"sa-secret"}`, Project: "proj-1", Location: "us-east5"}
	adc := &snapshot.Provider{ID: "pg", Name: "adc", Type: "gcp_vertex", AuthMode: "gcp_adc", Project: "proj-1"}
	entra := &snapshot.Provider{ID: "pz", Name: "foundry", Type: "azure", AuthMode: "entra", BaseURL: "https://r.openai.azure.com"}
	s.DeploymentsByModel["claude"] = []*snapshot.Deployment{
		{ID: "d-vertex", Provider: vertex, ModelName: "claude", UpstreamName: "claude-sonnet-4-5@20250929", APIFlavor: "anthropic"},
		{ID: "d-entra", Provider: entra, ModelName: "claude", UpstreamName: "claude-sonnet-4-5", APIFlavor: "anthropic"},
	}
	s.DeploymentsByModel["gemini"] = []*snapshot.Deployment{
		{ID: "d-adc", Provider: adc, ModelName: "gemini", UpstreamName: "google/gemini-2.5-flash"},
	}
	g := &snapshot.GatewayGroup{ID: "g", Name: "all"}

	old := buildConfig(s, g, 1)
	raw, _ := json.Marshal(old.cfg)
	for _, leak := range []string{"sa-secret", "proj-1", "d-vertex", "d-entra", "d-adc", `"claude"`, `"gemini"`} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("schema 1 config carries %s: %s", leak, raw)
		}
	}
	if len(old.cfg.Deployments) != 3 || len(old.cfg.Providers) != 3 {
		t.Errorf("schema 1 keeps the providers it can serve: %+v", old.cfg.Providers)
	}

	cur := buildConfig(s, g, schemaVersion)
	got := map[string]Provider{}
	for _, p := range cur.cfg.Providers {
		got[p.ID] = p
	}
	if p := got["pv"]; p.APIKey != vertex.APIKey || p.Project != "proj-1" || p.Location != "us-east5" || p.AuthMode != "gcp_sa" {
		t.Errorf("schema 2 vertex provider = %+v", p)
	}
	if got["pg"].AuthMode != "gcp_adc" || got["pz"].AuthMode != "entra" || got["pz"].APIKey != "" {
		t.Errorf("schema 2 providers = %+v", cur.cfg.Providers)
	}
	if len(cur.cfg.Deployments) != 6 {
		t.Errorf("schema 2 serves every deployment: %+v", cur.cfg.Deployments)
	}
}

func TestConfigRegionFirst(t *testing.T) {
	b := buildConfig(fixture(), &snapshot.GatewayGroup{ID: "g", Name: "eu", Region: "eu"}, schemaVersion)
	if b.cfg.Deployments[0].ID != "d-a-eu" || b.cfg.Deployments[1].ID != "d-a-us" {
		t.Fatalf("eu gateways should try the eu deployment first: %+v", b.cfg.Deployments)
	}
	b = buildConfig(fixture(), &snapshot.GatewayGroup{ID: "g", Name: "none"}, schemaVersion)
	if b.cfg.Deployments[0].ID != "d-a-us" {
		t.Fatalf("without a region the snapshot order stands: %+v", b.cfg.Deployments)
	}
}

func TestReachablePattern(t *testing.T) {
	id := &auth.Identity{Key: &snapshot.Key{AllowedModels: map[string]bool{"claude-sonnet-4": true}}}
	if !reachable(id, "claude-*") {
		t.Error("a key allowed a model under the pattern can reach it")
	}
	if reachable(id, "gpt-*") {
		t.Error("nothing under gpt-* is allowed")
	}
	if !reachable(&auth.Identity{}, "gpt-*") {
		t.Error("no policy reaches everything")
	}
}

// Sessions are signed with their own key: a console cookie, a tampered token
// or an expired one never opens a gateway endpoint.
func TestSessionRejects(t *testing.T) {
	s := &Server{Key: auth.DeriveKey("llmr-gw-session", "enc")}
	h := s.session(func(w http.ResponseWriter, r *http.Request, p peer) {
		w.Write([]byte(p.instance + "/" + p.group + "/" + strconv.Itoa(p.schema)))
	})
	call := func(token string) (int, string) {
		r := httptest.NewRequest("GET", "/gw/v1/config", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	good := auth.SignToken(s.Key, "i1:g1:2", time.Now().Add(time.Minute))
	if code, body := call(good); code != 200 || body != "i1/g1/2" {
		t.Fatalf("valid session: %d %s", code, body)
	}
	// A session from 0.2.x has no schema and speaks schema 1.
	if code, body := call(auth.SignToken(s.Key, "i1:g1", time.Now().Add(time.Minute))); code != 200 || body != "i1/g1/1" {
		t.Fatalf("0.2.x session: %d %s", code, body)
	}
	console := auth.SignToken(auth.DeriveKey("llmr-session", "enc"), "i1:g1", time.Now().Add(time.Minute))
	expired := auth.SignToken(s.Key, "i1:g1", time.Now().Add(-time.Second))
	for name, tok := range map[string]string{"console cookie": console, "expired": expired, "tampered": good[:len(good)-2] + "xx", "empty": ""} {
		if code, _ := call(tok); code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", name, code)
		}
	}
}

func withIssuer(s *snapshot.Snapshot, mapToTeam string, autoCreate bool) *snapshot.Snapshot {
	s.Issuers = []*snapshot.Issuer{{ID: "i1", Type: "oidc", IssuerURL: "https://idp.example.com", Audience: "aud",
		EmailClaim: "email", Match: map[string]string{"tid": "t"}, MapToTeam: mapToTeam, AutoCreateUser: autoCreate}}
	// dave has no key: only an identity token brings him to a gateway.
	dave := &snapshot.User{ID: "dave", Email: "Dave@Example.com", TeamIDs: []string{"A"}}
	s.UsersByID["dave"] = dave
	return s
}

func emailHash(email string) string {
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:])
}

// Identity-token callers need the issuers, an email index and users without
// keys; a schema 1 gateway can't use any of it and gets none.
func TestConfigIdentitySchemaGate(t *testing.T) {
	s := withIssuer(fixture(), "B", true)
	old := buildConfig(s, &snapshot.GatewayGroup{ID: "g", Name: "all"}, 1)
	raw, _ := json.Marshal(old.cfg)
	for _, f := range []string{`"issuers"`, `"emails"`, `"team_ids"`, `"dave"`} {
		if strings.Contains(string(raw), f) {
			t.Errorf("schema 1 config carries %s", f)
		}
	}

	b := buildConfig(s, &snapshot.GatewayGroup{ID: "g", Name: "all"}, 2)
	c := b.cfg
	if len(c.Issuers) != 1 || c.Issuers[0].URL != "https://idp.example.com" || !c.Issuers[0].AutoCreate ||
		c.Issuers[0].Match["tid"] != "t" || c.Issuers[0].MapToTeam != "B" {
		t.Fatalf("issuers = %+v", c.Issuers)
	}
	if len(c.Emails) != 4 || c.Emails[emailHash("dave@example.com")] != "dave" {
		t.Fatalf("emails = %v", c.Emails)
	}
	raw, _ = json.Marshal(c)
	if strings.Contains(strings.ToLower(string(raw)), "@example.com") {
		t.Fatal("a config carries an email address in the clear")
	}
	var dave *User
	for i := range c.Users {
		if c.Users[i].ID == "dave" {
			dave = &c.Users[i]
		}
	}
	if dave == nil || len(dave.TeamIDs) != 1 || dave.TeamIDs[0] != "A" {
		t.Fatalf("dave = %+v", dave)
	}

	// Without issuers nothing changes, so a deployment that doesn't use
	// identity tokens doesn't ship every user.
	plain := buildConfig(fixture(), &snapshot.GatewayGroup{ID: "g", Name: "all"}, 2)
	if plain.cfg.Issuers != nil || plain.cfg.Emails != nil || len(plain.cfg.Users) != 3 {
		t.Fatalf("no issuers: %+v %v %d", plain.cfg.Issuers, plain.cfg.Emails, len(plain.cfg.Users))
	}
}

// A team's gateway serves the members of its teams and nobody else, and only
// auto-creates users when they would join one of its teams.
func TestConfigIdentityForOneTeam(t *testing.T) {
	g := &snapshot.GatewayGroup{ID: "g", Name: "team-a", AllowedTeams: map[string]bool{"A": true}}
	b := buildConfig(withIssuer(fixture(), "B", true), g, 2)
	got := map[string]bool{}
	for _, id := range b.cfg.Emails {
		got[id] = true
	}
	if len(got) != 2 || !got["alice"] || !got["dave"] {
		t.Fatalf("team A's gateway serves %v, want alice and dave", got)
	}
	if b.cfg.Issuers[0].AutoCreate {
		t.Fatal("new users join team B, which this group doesn't serve: no auto-create")
	}
	// The issuer's team is where callers' usage counts, so its limits come along.
	teams := map[string]bool{}
	for _, t := range b.cfg.Teams {
		teams[t.ID] = true
	}
	if !teams["A"] || !teams["B"] {
		t.Fatalf("teams = %v", teams)
	}

	b = buildConfig(withIssuer(fixture(), "A", true), g, 2)
	if !b.cfg.Issuers[0].AutoCreate {
		t.Fatal("new users join team A, which this group serves: auto-create")
	}
}

// The sync check takes usage from a member of an allowed team, whatever team
// the issuer maps them to, and not from anyone else.
func TestMemberOfAllowed(t *testing.T) {
	s := withIssuer(fixture(), "B", false)
	g := &snapshot.GatewayGroup{ID: "g", AllowedTeams: map[string]bool{"A": true}}
	if !memberOfAllowed(s, g, "dave") || !memberOfAllowed(s, g, "alice") {
		t.Fatal("members of team A are this group's")
	}
	if memberOfAllowed(s, g, "bob") || memberOfAllowed(s, g, "carol") || memberOfAllowed(s, g, "") {
		t.Fatal("bob, carol and nobody are not this group's")
	}
}

// Schema 2 gateways get the capture policy and the content scopes of the
// destinations that take their group with content; never a destination's
// endpoint or credentials. Schema 1 gets neither.
func TestConfigTelemetry(t *testing.T) {
	s := fixture()
	s.Telemetry = snapshot.Telemetry{Mode: "by_rule", Rules: []snapshot.TelemetryRule{
		{ID: "r2", Order: 2, ScopeType: "tag", ScopeValue: "prod", Capture: "content", Sample: 50, Enabled: true},
		{ID: "r1", Order: 1, ScopeType: "team", ScopeValue: "A", Capture: "meta", Sample: 100, Enabled: true},
	}}
	s.KeysByHash[[32]byte{1}].Tags = []string{"prod", "eu"}
	s.TeamsByID["A"].Tags = []string{"team-tag"}
	content := func(name string, max int, teams, groups snapshot.Scope) *snapshot.Destination {
		return &snapshot.Destination{Name: name, Kind: "otlp", Enabled: true, Endpoint: "https://collector.example/" + name,
			Secrets: map[string]string{"header:authorization": "Bearer dest-secret"}, ContentMode: "content",
			MaxContentBytes: max, Teams: teams, Groups: groups}
	}
	all := snapshot.Scope{Mode: "all"}
	onlyA := snapshot.Scope{Mode: "selected", Values: []string{"A"}}
	s.Destinations = append(s.Destinations,
		content("small", 1000, all, all),
		content("big", 5000, all, snapshot.Scope{Mode: "selected", Values: []string{"g"}}),
		content("team-a", 300, onlyA, all),
		content("other-group", 9000, all, snapshot.Scope{Mode: "except", Values: []string{"g"}}),
		&snapshot.Destination{Name: "meta", Enabled: true, ContentMode: "metadata_only", MaxContentBytes: 9000},
		&snapshot.Destination{Name: "off", Enabled: false, ContentMode: "content", MaxContentBytes: 9000},
		&snapshot.Destination{Name: "broken", Enabled: true, ContentMode: "content", MaxContentBytes: 9000, Err: "bad"},
	)
	g := &snapshot.GatewayGroup{ID: "g", Name: "all", DataRule: "inherit"}

	old := buildConfig(s, g, 1)
	raw, _ := json.Marshal(old.cfg)
	for _, leak := range []string{`"telemetry"`, `"tags"`, "prod"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("schema 1 config carries %s", leak)
		}
	}

	c := buildConfig(s, g, 2).cfg
	tel := c.Telemetry
	if tel == nil || tel.Mode != "by_rule" || len(tel.Rules) != 2 || tel.Rules[0].ID != "r1" || tel.Rules[1].Sample != 50 {
		t.Fatalf("telemetry = %+v", tel)
	}
	want := []ContentScope{
		{Teams: snapshot.Scope{Mode: "all", Values: []string{}}, MaxBytes: 5000},
		{Teams: snapshot.Scope{Mode: "selected", Values: []string{"A"}}, MaxBytes: 300},
	}
	if len(tel.Content) != len(want) {
		t.Fatalf("content = %+v, want %+v", tel.Content, want)
	}
	for i := range want {
		if tel.Content[i].Teams.Mode != want[i].Teams.Mode || strings.Join(tel.Content[i].Teams.Values, ",") !=
			strings.Join(want[i].Teams.Values, ",") || tel.Content[i].MaxBytes != want[i].MaxBytes {
			t.Fatalf("content = %+v, want %+v", tel.Content, want)
		}
	}
	raw, _ = json.Marshal(c)
	for _, leak := range []string{"dest-secret", "collector.example", "lf-secret"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("schema 2 config carries %s", leak)
		}
	}
	for _, k := range c.Keys {
		if k.ID == "k-alice-team" && strings.Join(k.Tags, ",") != "eu,prod" {
			t.Errorf("key tags = %v", k.Tags)
		}
	}
	for _, tm := range c.Teams {
		if tm.ID == "A" && strings.Join(tm.Tags, ",") != "team-tag" {
			t.Errorf("team tags = %v", tm.Tags)
		}
	}

	// A group that keeps content in captures none, whatever the destinations.
	for _, rule := range []string{"no_content", "no_export"} {
		c := buildConfig(s, &snapshot.GatewayGroup{ID: "g", Name: "all", DataRule: rule}, 2).cfg
		if c.Telemetry == nil || len(c.Telemetry.Content) != 0 || c.Telemetry.Mode != "by_rule" {
			t.Errorf("%s: telemetry = %+v", rule, c.Telemetry)
		}
	}
	// No destinations and the default policy: an empty, not a missing, list.
	raw, _ = json.Marshal(buildConfig(fixture(), g, 2).cfg.Telemetry)
	if string(raw) != `{"mode":"","capture_content":false,"rules":[],"content":[]}` {
		t.Errorf("default telemetry = %s", raw)
	}
}

type collected struct{ events []usage.Event }

func (c *collected) Write(e usage.Event) { c.events = append(c.events, e) }

// The control plane decides export, and the gateway's verdict and the group's
// data rule can only take away: a caller's unsampled trace stays unexported.
func TestExportVerdicts(t *testing.T) {
	snap := fixture()
	snap.Telemetry = snapshot.Telemetry{Mode: "everything", CaptureContent: true}
	for _, c := range []struct {
		rule            string
		gateway         bool
		export, content bool
	}{
		{"inherit", true, true, true},
		{"inherit", false, false, true},
		{"no_content", true, true, false},
		{"no_export", true, false, false},
	} {
		out := &collected{}
		s := &Server{Export: out}
		g := &snapshot.GatewayGroup{ID: "g", DataRule: c.rule}
		s.export(snap, g, &encoded{built: built{keys: map[string]*snapshot.Key{}}}, usage.Event{RequestID: "r",
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", Export: c.gateway, CaptureContent: true, Output: "answer"})
		e := out.events[0]
		if e.Export != c.export || (e.Output != "") != c.content {
			t.Errorf("%+v: export %v, output %q", c, e.Export, e.Output)
		}
	}
}
