// Package snapshot holds the immutable in-memory config the gateway hot path
// reads from. It is rebuilt wholesale on any config change and swapped
// atomically — the hot path never touches Postgres.
// ponytail: full rebuild on any change; go incremental if key count reaches ~1e5.
package snapshot

import (
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

type Key struct {
	ID            string
	Prefix        string
	Name          string
	UserID        string
	TeamID        string
	AllowedModels map[string]bool // nil = all models
	BudgetUSD     *float64
	BudgetPeriod  string
	RPMLimit      *int
	TPMLimit      *int
	ExpiresAt     *time.Time
	Tags          []string
}

type User struct {
	ID            string
	Email         string
	Role          string
	AllowedModels map[string]bool // nil = no policy of their own
	// TeamModels is the union of the policies of the teams this user is in.
	// nil = unrestricted: no team, or at least one team with no policy.
	TeamModels map[string]bool
	TeamIDs    []string
	// Memberships carries the per-team budget for this user, by team id.
	Memberships  map[string]*Membership
	BudgetUSD    *float64
	BudgetPeriod string
	RPMLimit     *int
	TPMLimit     *int
	Tags         []string
}

// Membership is one user's seat on one team: their share of that team's
// budget. Spend through the team's keys by this member counts against it.
type Membership struct {
	BudgetUSD    *float64
	BudgetPeriod string
}

type Team struct {
	ID            string
	Name          string
	AllowedModels map[string]bool // nil = all models
	BudgetUSD     *float64
	BudgetPeriod  string
	RPMLimit      *int
	TPMLimit      *int
	Tags          []string
}

type Provider struct {
	ID         string
	Name       string
	Type       string // azure | openai_compatible | gcp_vertex | openrouter | anthropic
	BaseURL    string
	AuthMode   string // entra | api_key | bearer | none | oauth_passthrough | gcp_adc | gcp_sa | anthropic_wif
	APIKey     string // decrypted; the service account JSON for gcp_sa
	APIVersion string // azure only
	Project    string // gcp_vertex only
	Location   string // gcp_vertex only, e.g. us-east5 or global
	WIF        AnthropicWIF
}

// ApplyConfig fills in what a provider keeps in its config column. Everything that
// builds a Provider from a row goes through here, so none of them misses a field.
func (p *Provider) ApplyConfig(cfg map[string]any) {
	s := func(k string) string { v, _ := cfg[k].(string); return v }
	p.APIVersion, p.Project, p.Location = s("api_version"), s("project"), s("location")
	if p.AuthMode == "anthropic_wif" {
		p.WIF = AnthropicWIFFromConfig(cfg)
	}
}

// AnthropicWIF identifies a federation rule in the Claude Console. None of it is
// secret; the credential is the identity token on the machine serving the request.
type AnthropicWIF struct {
	FederationRuleID  string `json:"federation_rule_id"`
	OrganizationID    string `json:"organization_id"`
	ServiceAccountID  string `json:"service_account_id"`
	WorkspaceID       string `json:"workspace_id,omitempty"`
	IdentityTokenFile string `json:"identity_token_file,omitempty"`
}

// AnthropicWIFFromConfig reads the settings from a provider's config column.
func AnthropicWIFFromConfig(cfg map[string]any) AnthropicWIF {
	s := func(k string) string { v, _ := cfg[k].(string); return v }
	return AnthropicWIF{FederationRuleID: s("federation_rule_id"), OrganizationID: s("organization_id"),
		ServiceAccountID: s("service_account_id"), WorkspaceID: s("workspace_id"), IdentityTokenFile: s("identity_token_file")}
}

type Deployment struct {
	ID               string
	Provider         *Provider
	ModelName        string
	UpstreamName     string
	APIFlavor        string // openai | anthropic
	Priority         int
	CatalogModelID   string
	InputPer1M       *float64 // custom price override
	OutputPer1M      *float64
	CachedInputPer1M *float64 // optional; unset bills cache reads at the input rate
	Region           string   // gateways in a group with this region try it first
	TimeoutMS        *int     // wait for response headers before failing over; nil = default
}

// GatewayGroup is a set of data-plane gateways sharing a join token. It
// decides who may use them, which region they prefer and what telemetry may
// leave them; limits and budgets stay on keys, users and teams.
type GatewayGroup struct {
	ID           string
	Name         string
	Region       string
	AllowedTeams map[string]bool // nil = every team
	DataRule     string          // inherit | no_content | no_export
}

type Price struct {
	InputPer1M       float64
	OutputPer1M      float64
	CachedInputPer1M *float64
	// Above LongPromptTokens prompt tokens every rate is multiplied. 0 = no tier.
	LongPromptTokens     int
	LongPromptMultiplier float64
}

type Issuer struct {
	ID        string
	Type      string // entra | gcp
	IssuerURL string
	Audience  string
	// claim mapping
	EmailClaim     string
	Match          map[string]string
	MapToTeam      string
	AutoCreateUser bool
}

// Spend is the cached current-period spend per scope, refreshed with the snapshot.
type Spend struct {
	USD float64
}

type Snapshot struct {
	Version            int64
	KeysByHash         map[[32]byte]*Key
	UsersByID          map[string]*User
	UsersByEmail       map[string]*User
	TeamsByID          map[string]*Team
	DeploymentsByModel map[string][]*Deployment // sorted by priority asc
	Prices             map[string]Price         // catalog model_id -> price
	Issuers            []*Issuer
	// Spend holds current-period spend keyed by "key:"+id / "user:"+id / "team:"+id.
	Spend         map[string]Spend
	Telemetry     Telemetry
	GatewayGroups map[string]*GatewayGroup
	Destinations  []*Destination // by name
}

// Holder is the atomic snapshot pointer the hot path reads through.
type Holder struct{ p atomic.Pointer[Snapshot] }

func (h *Holder) Get() *Snapshot  { return h.p.Load() }
func (h *Holder) Set(s *Snapshot) { h.p.Store(s) }

// Rate is a deployment's list price per 1M tokens (input + output), used to
// order backends that share a priority so the cheaper one is tried first.
// ok is false when nothing prices it, which sorts last: an unknown cost must
// not beat a known one.
func (s *Snapshot) Rate(d *Deployment) (rate float64, ok bool) {
	switch {
	case d.InputPer1M != nil && d.OutputPer1M != nil:
		return *d.InputPer1M + *d.OutputPer1M, true
	case d.CatalogModelID != "":
		if p, found := s.Prices[d.CatalogModelID]; found {
			return p.InputPer1M + p.OutputPer1M, true
		}
	}
	return 0, false
}

// SortDeployments fixes the routing order per model: priority first, then
// price (cheapest first, unpriced last), then name so the order is stable.
func (s *Snapshot) SortDeployments() {
	for _, ds := range s.DeploymentsByModel {
		sort.SliceStable(ds, func(i, j int) bool {
			a, b := ds[i], ds[j]
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			ra, oka := s.Rate(a)
			rb, okb := s.Rate(b)
			if oka != okb {
				return oka
			}
			if oka && ra != rb {
				return ra < rb
			}
			return a.UpstreamName < b.UpstreamName
		})
	}
}

// ModelNames returns the distinct model names a client can send as they are,
// for /v1/models and model pickers. Patterns are left out: "claude-*" is not a
// model id, and a client that sent it would reach no upstream model.
func (s *Snapshot) ModelNames() []string {
	names := make([]string, 0, len(s.DeploymentsByModel))
	for name := range s.DeploymentsByModel {
		if !IsPattern(name) {
			names = append(names, name)
		}
	}
	return names
}

// IsPattern reports whether a model name is a prefix pattern such as "claude-*".
func IsPattern(name string) bool { return strings.HasSuffix(name, "*") }

// Resolve returns the model a requested name maps to and its backends in
// routing order. An exact name wins; otherwise the longest pattern whose prefix
// matches, so "claude-*" serves Claude models that did not exist when it was
// added. A pattern backend whose upstream name also ends in "*" is sent the
// rest of the requested name, on a per-request copy: snapshot rows are shared
// by every request in flight.
func (s *Snapshot) Resolve(name string) (string, []*Deployment) {
	if IsPattern(name) {
		return name, nil
	}
	if ds := s.DeploymentsByModel[name]; len(ds) > 0 {
		return name, ds
	}
	// ponytail: scans every model name on an exact miss, well under a microsecond
	// for hundreds of names; precompute the patterns in SortDeployments if that grows.
	best := ""
	for p := range s.DeploymentsByModel {
		if IsPattern(p) && len(p) > len(best) && strings.HasPrefix(name, p[:len(p)-1]) {
			best = p
		}
	}
	if best == "" {
		return name, nil
	}
	ds := s.DeploymentsByModel[best]
	rest := name[len(best)-1:]
	out, copied := ds, false
	for i, d := range ds {
		if !IsPattern(d.UpstreamName) {
			continue
		}
		up := strings.TrimSuffix(d.UpstreamName, "*") + rest
		// Azure puts the upstream name in a URL path segment, sent with the
		// gateway's own credential: it must never become a dot segment.
		if up == "." || up == ".." {
			return name, nil
		}
		if !copied {
			out, copied = append([]*Deployment(nil), ds...), true
		}
		dd := *d
		dd.UpstreamName = up
		out[i] = &dd
	}
	return best, out
}

// CallerCredentialOnly reports whether every backend for name forwards the
// caller's own upstream token (oauth_passthrough). Such a model only works for
// a client that sends that token — Claude Code on a subscription — so pickers
// for callers without one (the Playground, workspace agents, keys sent in
// Authorization) hide it instead of offering a request that can only 400.
func (s *Snapshot) CallerCredentialOnly(name string) bool {
	ds := s.DeploymentsByModel[name]
	if len(ds) == 0 {
		return false
	}
	for _, d := range ds {
		if d.Provider == nil || d.Provider.AuthMode != "oauth_passthrough" {
			return false
		}
	}
	return true
}
