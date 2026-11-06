package gwsync

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strings"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// schemaVersion is the newest config and sync wire format. A gateway that
// sends a newer one is refused at join, so an old control plane never
// half-serves it. Older gateways get the schema they speak: 1 is 0.2.x.
// Schema 2 adds Vertex and Entra credentials, identity-token issuers and the
// telemetry policy, which a schema 1 gateway can't use and so never receives.
const schemaVersion = 2

// Config is what a gateway runs on: the keys, users and teams its group
// serves, the deployments they can reach in the group's routing order, and the
// prices to cost requests with. Spend and telemetry credentials are not part
// of it; spend comes with every sync.
type Config struct {
	Schema      int              `json:"schema"`
	Version     int64            `json:"version"`
	Group       Group            `json:"group"`
	Keys        []Key            `json:"keys"`
	Users       []User           `json:"users"`
	Teams       []Team           `json:"teams"`
	Providers   []Provider       `json:"providers"`
	Deployments []Deployment     `json:"deployments"` // per model, in routing order
	Prices      map[string]Price `json:"prices"`
	// Schema 2, and only when issuers are configured: identity-token issuers,
	// and every user the group serves by hex SHA-256 of their lowercased email,
	// so a gateway can map a verified token to a user without the address.
	Issuers []Issuer          `json:"issuers,omitempty"`
	Emails  map[string]string `json:"emails,omitempty"`
	// Schema 2: the capture policy and what content the group's gateways
	// capture. Destinations and their credentials stay on the control plane.
	Telemetry *Telemetry `json:"telemetry,omitempty"`
}

// Telemetry is the capture policy (snapshot.Telemetry) plus Content: the
// content scope of every usable destination that takes this group's traffic
// with content. A gateway captures content for a request only when the policy
// captures it and a scope here takes the caller's team, up to the largest
// MaxBytes that does (snapshot.ContentWanted). Empty when the group's data
// rule keeps content in. The control plane decides again what each
// destination receives; this only spares gateways capturing what none would.
type Telemetry struct {
	Mode           string                   `json:"mode"`
	CaptureContent bool                     `json:"capture_content"`
	Rules          []snapshot.TelemetryRule `json:"rules"` // by order
	Content        []ContentScope           `json:"content"`
}

// ContentScope is the teams a content destination takes, and its cap.
type ContentScope struct {
	Teams    snapshot.Scope `json:"teams"`
	MaxBytes int            `json:"max_bytes"`
}

// Issuer is a token issuer a gateway verifies callers' identity tokens
// against (snapshot.Issuer). AutoCreate is set only when a user it creates
// would be served by this group.
type Issuer struct {
	URL        string            `json:"url"`
	Audience   string            `json:"audience"`
	EmailClaim string            `json:"email_claim"`
	Match      map[string]string `json:"match,omitempty"`
	MapToTeam  string            `json:"map_to_team,omitempty"`
	AutoCreate bool              `json:"auto_create,omitempty"`
}

type Group struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Region   string `json:"region"`
	DataRule string `json:"data_rule"`
}

// Model lists are null when they don't restrict and [] when they admit nothing.
// Tags (schema 2) are for the telemetry policy's tag rules.
type Key struct {
	Hash          string   `json:"hash"` // hex SHA-256 of the secret
	ID            string   `json:"id"`
	UserID        string   `json:"user_id,omitempty"`
	TeamID        string   `json:"team_id,omitempty"`
	AllowedModels []string `json:"allowed_models"`
	BudgetUSD     *float64 `json:"budget_usd,omitempty"`
	RPMLimit      *int     `json:"rpm_limit,omitempty"`
	TPMLimit      *int     `json:"tpm_limit,omitempty"`
	ExpiresAt     int64    `json:"expires_at,omitempty"` // unix seconds
	Tags          []string `json:"tags,omitempty"`
}

type User struct {
	ID            string              `json:"id"`
	AllowedModels []string            `json:"allowed_models"`        // the user's own policy
	TeamModels    []string            `json:"team_models"`           // union of their teams' policies
	Memberships   map[string]*float64 `json:"memberships,omitempty"` // team id → budget share
	BudgetUSD     *float64            `json:"budget_usd,omitempty"`
	RPMLimit      *int                `json:"rpm_limit,omitempty"`
	TPMLimit      *int                `json:"tpm_limit,omitempty"`
	TeamIDs       []string            `json:"team_ids,omitempty"` // schema 2
	Tags          []string            `json:"tags,omitempty"`     // schema 2
}

type Team struct {
	ID            string   `json:"id"`
	AllowedModels []string `json:"allowed_models"`
	BudgetUSD     *float64 `json:"budget_usd,omitempty"`
	RPMLimit      *int     `json:"rpm_limit,omitempty"`
	TPMLimit      *int     `json:"tpm_limit,omitempty"`
	Tags          []string `json:"tags,omitempty"` // schema 2
}

type Provider struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	BaseURL    string `json:"base_url"`
	AuthMode   string `json:"auth_mode"`
	APIKey     string `json:"api_key,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
	Project    string `json:"project,omitempty"`
	Location   string `json:"location,omitempty"`
	// Each gateway exchanges its own identity token; no credential is sent.
	WIF *snapshot.AnthropicWIF `json:"wif,omitempty"`
}

type Deployment struct {
	ID               string   `json:"id"`
	ProviderID       string   `json:"provider_id"`
	ModelName        string   `json:"model_name"`
	UpstreamName     string   `json:"upstream_name"`
	APIFlavor        string   `json:"api_flavor"`
	TimeoutMS        *int     `json:"timeout_ms,omitempty"`
	CatalogModelID   string   `json:"catalog_model_id,omitempty"`
	InputPer1M       *float64 `json:"input_per_1m,omitempty"`
	OutputPer1M      *float64 `json:"output_per_1m,omitempty"`
	CachedInputPer1M *float64 `json:"cached_input_per_1m,omitempty"`
}

type Price struct {
	InputPer1M           float64  `json:"input_per_1m"`
	OutputPer1M          float64  `json:"output_per_1m"`
	CachedInputPer1M     *float64 `json:"cached_input_per_1m,omitempty"`
	LongPromptTokens     int      `json:"long_prompt_tokens,omitempty"`
	LongPromptMultiplier float64  `json:"long_prompt_multiplier,omitempty"`
}

// built is a group's config plus what the sync handler needs to check the
// usage its gateways upload.
type built struct {
	cfg  Config
	keys map[string]*snapshot.Key // by id: keys the group serves
}

// buildConfig cuts the snapshot down to what group g's gateways serve. A group
// limited to some teams gets their keys (and the personal keys of their
// members), those users and teams, and only the deployments those callers can
// reach, so a team's gateway never holds credentials for another team's models.
func buildConfig(s *snapshot.Snapshot, g *snapshot.GatewayGroup, schema int) built {
	b := built{keys: map[string]*snapshot.Key{}}
	c := &b.cfg
	c.Schema, c.Version = schema, s.Version
	// Empty lists, not null, so a gateway never has to tell the two apart.
	c.Keys, c.Users, c.Teams, c.Providers, c.Deployments = []Key{}, []User{}, []Team{}, []Provider{}, []Deployment{}
	c.Group = Group{ID: g.ID, Name: g.Name, Region: g.Region, DataRule: g.DataRule}

	inGroup := func(k *snapshot.Key) bool {
		if g.AllowedTeams == nil {
			return true
		}
		if k.TeamID != "" {
			return g.AllowedTeams[k.TeamID]
		}
		if u := s.UsersByID[k.UserID]; u != nil {
			for _, t := range u.TeamIDs {
				if g.AllowedTeams[t] {
					return true
				}
			}
		}
		return false
	}
	users := map[string]*snapshot.User{}
	teams := map[string]*snapshot.Team{}
	var ids []*auth.Identity
	for hash, k := range s.KeysByHash {
		if !inGroup(k) {
			continue
		}
		u := s.UsersByID[k.UserID]
		if k.UserID != "" && u == nil {
			continue // a disabled user's keys don't work anywhere
		}
		t := s.TeamsByID[k.TeamID]
		if u != nil {
			users[u.ID] = u
		}
		if t != nil {
			teams[t.ID] = t
		}
		b.keys[k.ID] = k
		ids = append(ids, &auth.Identity{Key: k, User: u, Team: t})
		wk := Key{Hash: hex.EncodeToString(hash[:]), ID: k.ID, UserID: k.UserID, TeamID: k.TeamID,
			AllowedModels: list(k.AllowedModels), BudgetUSD: k.BudgetUSD, RPMLimit: k.RPMLimit, TPMLimit: k.TPMLimit}
		if k.ExpiresAt != nil {
			wk.ExpiresAt = k.ExpiresAt.Unix()
		}
		if schema >= 2 {
			wk.Tags = tags(k.Tags)
		}
		c.Keys = append(c.Keys, wk)
	}
	if schema >= 2 && len(s.Issuers) > 0 {
		ids = addIdentityUsers(c, s, g, users, teams, ids)
	}
	for _, u := range users {
		wu := User{ID: u.ID, AllowedModels: list(u.AllowedModels), TeamModels: list(u.TeamModels),
			BudgetUSD: u.BudgetUSD, RPMLimit: u.RPMLimit, TPMLimit: u.TPMLimit}
		for teamID, m := range u.Memberships {
			if m.BudgetUSD != nil {
				if wu.Memberships == nil {
					wu.Memberships = map[string]*float64{}
				}
				wu.Memberships[teamID] = m.BudgetUSD
			}
		}
		if schema >= 2 && len(u.TeamIDs) > 0 {
			wu.TeamIDs = slices.Sorted(slices.Values(u.TeamIDs))
		}
		if schema >= 2 {
			wu.Tags = tags(u.Tags)
		}
		c.Users = append(c.Users, wu)
	}
	for _, t := range teams {
		wt := Team{ID: t.ID, AllowedModels: list(t.AllowedModels),
			BudgetUSD: t.BudgetUSD, RPMLimit: t.RPMLimit, TPMLimit: t.TPMLimit}
		if schema >= 2 {
			wt.Tags = tags(t.Tags)
		}
		c.Teams = append(c.Teams, wt)
	}
	if schema >= 2 {
		c.Telemetry = telemetryFor(s, g)
	}

	names := make([]string, 0, len(s.DeploymentsByModel))
	for name := range s.DeploymentsByModel {
		if g.AllowedTeams == nil || slices.ContainsFunc(ids, func(id *auth.Identity) bool { return reachable(id, name) }) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	providers := map[string]*snapshot.Provider{}
	for _, name := range names {
		for _, d := range routingOrder(s.DeploymentsByModel[name], g.Region) {
			if !Servable(d.Provider, schema) {
				continue
			}
			providers[d.Provider.ID] = d.Provider
			c.Deployments = append(c.Deployments, Deployment{ID: d.ID, ProviderID: d.Provider.ID,
				ModelName: d.ModelName, UpstreamName: d.UpstreamName, APIFlavor: d.APIFlavor, TimeoutMS: d.TimeoutMS,
				CatalogModelID: d.CatalogModelID, InputPer1M: d.InputPer1M, OutputPer1M: d.OutputPer1M,
				CachedInputPer1M: d.CachedInputPer1M})
		}
	}
	for _, p := range providers {
		wp := Provider{ID: p.ID, Name: p.Name, Type: p.Type, BaseURL: p.BaseURL,
			AuthMode: p.AuthMode, APIKey: p.APIKey, APIVersion: p.APIVersion, Project: p.Project, Location: p.Location}
		if p.AuthMode == "anthropic_wif" {
			wif := p.WIF
			wp.WIF = &wif
		}
		c.Providers = append(c.Providers, wp)
	}
	c.Prices = make(map[string]Price, len(s.Prices))
	for id, p := range s.Prices {
		c.Prices[id] = Price{InputPer1M: p.InputPer1M, OutputPer1M: p.OutputPer1M, CachedInputPer1M: p.CachedInputPer1M,
			LongPromptTokens: p.LongPromptTokens, LongPromptMultiplier: p.LongPromptMultiplier}
	}

	// Stable order, so a config only changes when its content does.
	sort.Slice(c.Keys, func(i, j int) bool { return c.Keys[i].ID < c.Keys[j].ID })
	sort.Slice(c.Users, func(i, j int) bool { return c.Users[i].ID < c.Users[j].ID })
	sort.Slice(c.Teams, func(i, j int) bool { return c.Teams[i].ID < c.Teams[j].ID })
	sort.Slice(c.Providers, func(i, j int) bool { return c.Providers[i].ID < c.Providers[j].ID })
	return b
}

// telemetryFor is the policy, and the content scopes of the destinations that
// take group g's traffic with content: none when its data rule keeps content
// in. Equal team scopes merge, keeping the larger cap.
func telemetryFor(s *snapshot.Snapshot, g *snapshot.GatewayGroup) *Telemetry {
	t := &Telemetry{Mode: s.Telemetry.Mode, CaptureContent: s.Telemetry.CaptureContent,
		Rules: append([]snapshot.TelemetryRule{}, s.Telemetry.Rules...), Content: []ContentScope{}}
	sort.SliceStable(t.Rules, func(i, j int) bool { return t.Rules[i].Order < t.Rules[j].Order })
	if g.DataRule != "" && g.DataRule != "inherit" {
		return t
	}
	byScope := map[string]int{} // scope → index in t.Content
	for _, d := range s.Destinations {
		if !d.Usable() || d.ContentMode != "content" || !d.Groups.Match(g.ID) {
			continue
		}
		teams := snapshot.Scope{Mode: d.Teams.Mode, Values: slices.Sorted(slices.Values(d.Teams.Values))}
		switch teams.Mode {
		case "selected", "except":
		default:
			teams = snapshot.Scope{Mode: "all", Values: []string{}}
		}
		if teams.Values == nil {
			teams.Values = []string{}
		}
		k := teams.Mode + "\x00" + strings.Join(teams.Values, "\x00")
		if i, ok := byScope[k]; ok {
			t.Content[i].MaxBytes = max(t.Content[i].MaxBytes, d.MaxContentBytes)
			continue
		}
		byScope[k] = len(t.Content)
		t.Content = append(t.Content, ContentScope{Teams: teams, MaxBytes: d.MaxContentBytes})
	}
	sort.Slice(t.Content, func(i, j int) bool {
		a, b := t.Content[i].Teams, t.Content[j].Teams
		if a.Mode != b.Mode {
			return a.Mode < b.Mode
		}
		return strings.Join(a.Values, "\x00") < strings.Join(b.Values, "\x00")
	})
	return t
}

// tags is a sorted copy, nil when empty.
func tags(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return slices.Sorted(slices.Values(in))
}

// Servable reports whether a gateway speaking schema can serve p. Schema 1
// gateways (0.2.x) mint no Google or Entra tokens and know no Vertex URLs, so
// they get neither these providers nor their deployments, and never hold a
// service account key they can't use.
func Servable(p *snapshot.Provider, schema int) bool {
	if schema >= 2 {
		return true
	}
	switch p.AuthMode {
	case "entra", "gcp_adc", "gcp_sa":
		return false
	}
	return p.Type != "gcp_vertex"
}

// servesUser reports whether group g serves u when they sign in with an
// identity token: every user for an unrestricted group, else members of a
// team the group allows.
func servesUser(g *snapshot.GatewayGroup, u *snapshot.User) bool {
	if g.AllowedTeams == nil {
		return true
	}
	return slices.ContainsFunc(u.TeamIDs, func(t string) bool { return g.AllowedTeams[t] })
}

// memberOfAllowed reports whether the user is a member of a team the group
// allows: an identity-token caller the group serves, whatever team the
// issuer maps them to.
func memberOfAllowed(snap *snapshot.Snapshot, g *snapshot.GatewayGroup, userID string) bool {
	u := snap.UsersByID[userID]
	return u != nil && servesUser(g, u)
}

// addIdentityUsers adds what identity-token callers need: the issuers, every
// user the group serves with their email hash, and the teams the issuers map
// callers to. It returns ids with the identities those callers can have, so
// the deployments they can reach are part of the config.
func addIdentityUsers(c *Config, s *snapshot.Snapshot, g *snapshot.GatewayGroup,
	users map[string]*snapshot.User, teams map[string]*snapshot.Team, ids []*auth.Identity) []*auth.Identity {
	var issuerTeams []*snapshot.Team // nil for an issuer that maps callers to no team
	for _, iss := range s.Issuers {
		wi := Issuer{URL: iss.IssuerURL, Audience: iss.Audience, EmailClaim: iss.EmailClaim, Match: iss.Match,
			MapToTeam:  iss.MapToTeam,
			AutoCreate: iss.AutoCreateUser && (g.AllowedTeams == nil || g.AllowedTeams[iss.MapToTeam])}
		c.Issuers = append(c.Issuers, wi)
		t := s.TeamsByID[iss.MapToTeam]
		if t != nil {
			teams[t.ID] = t
		}
		if !slices.Contains(issuerTeams, t) {
			issuerTeams = append(issuerTeams, t)
		}
	}
	sort.Slice(c.Issuers, func(i, j int) bool { return c.Issuers[i].URL < c.Issuers[j].URL })
	c.Emails = map[string]string{}
	for _, u := range s.UsersByID {
		if !servesUser(g, u) {
			continue
		}
		users[u.ID] = u
		sum := sha256.Sum256([]byte(strings.ToLower(u.Email)))
		c.Emails[hex.EncodeToString(sum[:])] = u.ID
		if g.AllowedTeams != nil {
			for _, t := range issuerTeams {
				ids = append(ids, &auth.Identity{User: u, Team: t})
			}
		}
	}
	// An auto-created caller has no policy of their own yet: their team's.
	if g.AllowedTeams != nil {
		for _, iss := range c.Issuers {
			if iss.AutoCreate {
				ids = append(ids, &auth.Identity{User: &snapshot.User{}, Team: s.TeamsByID[iss.MapToTeam]})
			}
		}
	}
	return ids
}

// routingOrder puts the deployments in the group's region first, keeping the
// snapshot's priority-then-price order inside each part.
func routingOrder(ds []*snapshot.Deployment, region string) []*snapshot.Deployment {
	if region == "" {
		return ds
	}
	out := make([]*snapshot.Deployment, 0, len(ds))
	for _, d := range ds {
		if d.Region == region {
			out = append(out, d)
		}
	}
	for _, d := range ds {
		if d.Region != region {
			out = append(out, d)
		}
	}
	return out
}

// reachable reports whether a caller could ever be routed to the model name. A
// pattern ("claude-*") counts when every policy level allows it or something
// under it, since the caller sends concrete names.
func reachable(id *auth.Identity, name string) bool {
	if !snapshot.IsPattern(name) {
		return id.ModelAllowed(name)
	}
	prefix := strings.TrimSuffix(name, "*")
	admits := func(policy map[string]bool) bool {
		if policy == nil {
			return true
		}
		for m := range policy {
			if m == name || strings.HasPrefix(m, prefix) {
				return true
			}
		}
		return false
	}
	if id.Team != nil && !admits(id.Team.AllowedModels) {
		return false
	}
	if u := id.User; u != nil {
		policy := u.AllowedModels
		if policy == nil {
			policy = u.TeamModels
		}
		if !admits(policy) {
			return false
		}
	}
	return id.Key == nil || admits(id.Key.AllowedModels)
}

// list turns a model set into a sorted list, keeping nil (no restriction)
// apart from empty (nothing allowed).
func list(m map[string]bool) []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
