package gwsync

import (
	"encoding/hex"
	"slices"
	"sort"
	"strings"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// schemaVersion is the config and sync wire format. A gateway that sends a
// newer one is refused at join, so an old control plane never half-serves it.
const schemaVersion = 1

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
}

type Group struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Region   string `json:"region"`
	DataRule string `json:"data_rule"`
}

// Model lists are null when they don't restrict and [] when they admit nothing.
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
}

type User struct {
	ID            string              `json:"id"`
	AllowedModels []string            `json:"allowed_models"`        // the user's own policy
	TeamModels    []string            `json:"team_models"`           // union of their teams' policies
	Memberships   map[string]*float64 `json:"memberships,omitempty"` // team id → budget share
	BudgetUSD     *float64            `json:"budget_usd,omitempty"`
	RPMLimit      *int                `json:"rpm_limit,omitempty"`
	TPMLimit      *int                `json:"tpm_limit,omitempty"`
}

type Team struct {
	ID            string   `json:"id"`
	AllowedModels []string `json:"allowed_models"`
	BudgetUSD     *float64 `json:"budget_usd,omitempty"`
	RPMLimit      *int     `json:"rpm_limit,omitempty"`
	TPMLimit      *int     `json:"tpm_limit,omitempty"`
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
	InputPer1M       float64  `json:"input_per_1m"`
	OutputPer1M      float64  `json:"output_per_1m"`
	CachedInputPer1M *float64 `json:"cached_input_per_1m,omitempty"`
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
func buildConfig(s *snapshot.Snapshot, g *snapshot.GatewayGroup) built {
	b := built{keys: map[string]*snapshot.Key{}}
	c := &b.cfg
	c.Schema, c.Version = schemaVersion, s.Version
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
		c.Keys = append(c.Keys, wk)
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
		c.Users = append(c.Users, wu)
	}
	for _, t := range teams {
		c.Teams = append(c.Teams, Team{ID: t.ID, AllowedModels: list(t.AllowedModels),
			BudgetUSD: t.BudgetUSD, RPMLimit: t.RPMLimit, TPMLimit: t.TPMLimit})
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
			providers[d.Provider.ID] = d.Provider
			c.Deployments = append(c.Deployments, Deployment{ID: d.ID, ProviderID: d.Provider.ID,
				ModelName: d.ModelName, UpstreamName: d.UpstreamName, APIFlavor: d.APIFlavor, TimeoutMS: d.TimeoutMS,
				CatalogModelID: d.CatalogModelID, InputPer1M: d.InputPer1M, OutputPer1M: d.OutputPer1M,
				CachedInputPer1M: d.CachedInputPer1M})
		}
	}
	for _, p := range providers {
		c.Providers = append(c.Providers, Provider{ID: p.ID, Name: p.Name, Type: p.Type, BaseURL: p.BaseURL,
			AuthMode: p.AuthMode, APIKey: p.APIKey, APIVersion: p.APIVersion, Project: p.Project, Location: p.Location})
	}
	c.Prices = make(map[string]Price, len(s.Prices))
	for id, p := range s.Prices {
		c.Prices[id] = Price{InputPer1M: p.InputPer1M, OutputPer1M: p.OutputPer1M, CachedInputPer1M: p.CachedInputPer1M}
	}

	// Stable order, so a config only changes when its content does.
	sort.Slice(c.Keys, func(i, j int) bool { return c.Keys[i].ID < c.Keys[j].ID })
	sort.Slice(c.Users, func(i, j int) bool { return c.Users[i].ID < c.Users[j].ID })
	sort.Slice(c.Teams, func(i, j int) bool { return c.Teams[i].ID < c.Teams[j].ID })
	sort.Slice(c.Providers, func(i, j int) bool { return c.Providers[i].ID < c.Providers[j].ID })
	return b
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
