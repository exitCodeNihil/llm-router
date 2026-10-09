package gwsync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
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
		Prices:    map[string]snapshot.Price{"gpt-a": {InputPer1M: 1, OutputPer1M: 2}},
		Spend:     map[string]snapshot.Spend{"team:B": {USD: 3}},
		Telemetry: snapshot.Telemetry{Enabled: true, SecretKey: "lf-secret"},
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
	b := buildConfig(fixture(), &snapshot.GatewayGroup{ID: "g", Name: "all"})
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
	b := buildConfig(fixture(), g)
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

func TestConfigRegionFirst(t *testing.T) {
	b := buildConfig(fixture(), &snapshot.GatewayGroup{ID: "g", Name: "eu", Region: "eu"})
	if b.cfg.Deployments[0].ID != "d-a-eu" || b.cfg.Deployments[1].ID != "d-a-us" {
		t.Fatalf("eu gateways should try the eu deployment first: %+v", b.cfg.Deployments)
	}
	b = buildConfig(fixture(), &snapshot.GatewayGroup{ID: "g", Name: "none"})
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
	h := s.session(func(w http.ResponseWriter, r *http.Request, instance, group string) {
		w.Write([]byte(instance + "/" + group))
	})
	call := func(token string) (int, string) {
		r := httptest.NewRequest("GET", "/gw/v1/config", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	good := auth.SignToken(s.Key, "i1:g1", time.Now().Add(time.Minute))
	if code, body := call(good); code != 200 || body != "i1/g1" {
		t.Fatalf("valid session: %d %s", code, body)
	}
	console := auth.SignToken(auth.DeriveKey("llmr-session", "enc"), "i1:g1", time.Now().Add(time.Minute))
	expired := auth.SignToken(s.Key, "i1:g1", time.Now().Add(-time.Second))
	for name, tok := range map[string]string{"console cookie": console, "expired": expired, "tampered": good[:len(good)-2] + "xx", "empty": ""} {
		if code, _ := call(tok); code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", name, code)
		}
	}
}
