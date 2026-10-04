// Package snapshot holds the immutable in-memory config the gateway hot path
// reads from. It is rebuilt wholesale on any config change and swapped
// atomically — the hot path never touches Postgres.
// ponytail: full rebuild on any change; go incremental if key count reaches ~1e5.
package snapshot

import (
	"math/rand"
	"sort"
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
	Type       string // azure | openai_compatible | gcp_vertex
	BaseURL    string
	AuthMode   string // entra | api_key | bearer | none | oauth_passthrough | gcp_adc | gcp_sa
	APIKey     string // decrypted; the service account JSON for gcp_sa
	APIVersion string // azure only
	Project    string // gcp_vertex only
	Location   string // gcp_vertex only, e.g. us-east5 or global
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
}

type Price struct {
	InputPer1M       float64
	OutputPer1M      float64
	CachedInputPer1M *float64
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

// Telemetry is the observability-exporter config (settings key 'telemetry').
// Generic by design: Type selects the backend (only "langfuse" today).
type Telemetry struct {
	Type           string `json:"type"`
	Host           string `json:"host"`
	PublicKey      string `json:"public_key"`
	SecretKey      string `json:"secret_key"`
	Enabled        bool   `json:"enabled"`
	CaptureContent bool   `json:"capture_content"` // default capture for "everything" mode
	// Mode selects how requests are exported:
	//   "everything" (default) — export all traffic, capture per CaptureContent
	//   "by_rule"    — first matching enabled rule (by Order) decides
	//   "off"        — export nothing
	Mode  string          `json:"mode,omitempty"`
	Rules []TelemetryRule `json:"rules,omitempty"`
}

// TelemetryRule is one ordered export rule (by_rule mode).
type TelemetryRule struct {
	ID         string `json:"id"`
	Order      int    `json:"order"`
	ScopeType  string `json:"scope_type"`  // team | user | key | tag
	ScopeValue string `json:"scope_value"` // id or tag string
	Capture    string `json:"capture"`     // content | meta
	Sample     int    `json:"sample"`      // 0..100, percent of matches exported
	Enabled    bool   `json:"enabled"`
}

// Decision is the per-request export verdict computed once post-auth.
type Decision struct {
	Export         bool
	CaptureContent bool
	RuleID         string
}

// Evaluate decides whether this request is exported and whether its content is
// captured. First enabled rule (by Order) whose scope matches wins.
func (t Telemetry) Evaluate(keyID, userID, teamID string, tags []string) Decision {
	if !t.Enabled {
		return Decision{}
	}
	switch t.Mode {
	case "off":
		return Decision{}
	case "by_rule":
		rules := append([]TelemetryRule(nil), t.Rules...)
		sort.SliceStable(rules, func(i, j int) bool { return rules[i].Order < rules[j].Order })
		for _, r := range rules {
			if !r.Enabled || !ruleMatches(r, keyID, userID, teamID, tags) {
				continue
			}
			if !sampleGate(r.Sample) {
				return Decision{RuleID: r.ID} // matched but sampled out
			}
			return Decision{Export: true, CaptureContent: r.Capture == "content", RuleID: r.ID}
		}
		return Decision{}
	default: // "" or "everything"
		return Decision{Export: true, CaptureContent: t.CaptureContent}
	}
}

func ruleMatches(r TelemetryRule, keyID, userID, teamID string, tags []string) bool {
	switch r.ScopeType {
	case "key":
		return r.ScopeValue == keyID
	case "user":
		return r.ScopeValue == userID
	case "team":
		return r.ScopeValue == teamID
	case "tag":
		for _, tag := range tags {
			if tag == r.ScopeValue {
				return true
			}
		}
	}
	return false
}

// sampleGate passes sample% of calls. Bounds are exact: 0 never, 100 always.
func sampleGate(sample int) bool {
	if sample >= 100 {
		return true
	}
	if sample <= 0 {
		return false
	}
	return rand.Float64()*100 < float64(sample) // ponytail: probabilistic per-request sampling
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
	Spend     map[string]Spend
	Telemetry Telemetry
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

// ModelNames returns the distinct public model names, for /v1/models.
func (s *Snapshot) ModelNames() []string {
	names := make([]string, 0, len(s.DeploymentsByModel))
	for name := range s.DeploymentsByModel {
		names = append(names, name)
	}
	return names
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
