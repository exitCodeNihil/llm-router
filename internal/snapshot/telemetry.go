package snapshot

import (
	"hash/fnv"
	"slices"
	"sort"
	"strings"
)

// Telemetry is the capture policy (settings key 'telemetry'). It applies
// before every destination: a request it doesn't export reaches none of them,
// and content it doesn't capture reaches none of them.
type Telemetry struct {
	// Mode selects how requests are exported:
	//   "everything" (default) — export all traffic, capture per CaptureContent
	//   "by_rule"    — first matching enabled rule (by Order) decides
	//   "off"        — export nothing
	Mode           string          `json:"mode"`
	CaptureContent bool            `json:"capture_content"` // for "everything" mode
	Rules          []TelemetryRule `json:"rules"`
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
func (t Telemetry) Evaluate(keyID, userID, teamID string, tags []string, requestID string) Decision {
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
			if !Sampled(r.ID, requestID, r.Sample) {
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
		return slices.Contains(tags, r.ScopeValue)
	}
	return false
}

// Sampled passes sample% of requests, the same ones on every control plane
// and gateway: FNV-1a 64 over "rule_id:request_id", modulo 100, below sample.
// Bounds are exact: 0 never, 100 always. testdata/sample.json pins it for the
// Rust port.
func Sampled(ruleID, requestID string, sample int) bool {
	if sample >= 100 {
		return true
	}
	if sample <= 0 {
		return false
	}
	return SampleBucket(ruleID, requestID) < uint64(sample)
}

// SampleBucket is the request's sampling bucket for a rule, 0..99.
func SampleBucket(ruleID, requestID string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(ruleID + ":" + requestID))
	return h.Sum64() % 100
}

// ControlPlaneGroup stands for traffic the control plane serves itself in
// destination group scopes.
const ControlPlaneGroup = "_control_plane"

// Scope selects teams or gateway groups: all of them, only Values, or all
// except Values.
type Scope struct {
	Mode   string   `json:"mode"` // all | selected | except
	Values []string `json:"values"`
}

func (s Scope) Match(v string) bool {
	switch s.Mode {
	case "selected":
		return slices.Contains(s.Values, v)
	case "except":
		return !slices.Contains(s.Values, v)
	}
	return true
}

// Destination is one place traces are exported to.
type Destination struct {
	ID        string
	Name      string
	Kind      string // otlp | langfuse | any registered kind
	Enabled   bool
	Endpoint  string
	TracesURL string            // overrides the kind's default traces path
	Config    map[string]string // public, kind-specific fields
	// Secrets are decrypted. Keys starting with HeaderSecretPrefix are extra
	// request headers; the rest are the kind's secret fields.
	Secrets         map[string]string
	ContentMode     string // metadata_only | content
	MaxContentBytes int
	Teams, Groups   Scope
	// Err is why the destination can't run, e.g. its secrets can't be
	// decrypted. Such a destination receives nothing.
	Err string
}

// HeaderSecretPrefix marks a secret that is sent as a request header.
const HeaderSecretPrefix = "header:"

// Headers returns the extra request headers, by name.
func (d *Destination) Headers() map[string]string {
	out := map[string]string{}
	for k, v := range d.Secrets {
		if name, ok := strings.CutPrefix(k, HeaderSecretPrefix); ok {
			out[name] = v
		}
	}
	return out
}

// SecretNames lists the secrets that are set, sorted.
func (d *Destination) SecretNames() []string {
	names := make([]string, 0, len(d.Secrets))
	for k := range d.Secrets {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Usable reports whether the destination receives traffic.
func (d *Destination) Usable() bool { return d.Enabled && d.Err == "" }

// Takes reports whether traffic from team through group is in its scope.
func (d *Destination) Takes(team, group string) bool {
	return d.Teams.Match(team) && d.Groups.Match(group)
}

// DataRule is what a gateway group lets leave it: inherit, no_content or
// no_export. The control plane, and a group not in the snapshot, inherit.
func (s *Snapshot) DataRule(group string) string {
	if g := s.GatewayGroups[group]; g != nil && g.DataRule != "" {
		return g.DataRule
	}
	return "inherit"
}

// ContentWanted is how many bytes of content to capture for traffic from team
// through group: the largest cap of the destinations that would take it with
// content, 0 when none would. The policy decides separately whether content
// may be captured at all.
func (s *Snapshot) ContentWanted(team, group string) int {
	if s.DataRule(group) != "inherit" {
		return 0
	}
	n := 0
	for _, d := range s.Destinations {
		if d.Usable() && d.ContentMode == "content" && d.Takes(team, group) {
			n = max(n, d.MaxContentBytes)
		}
	}
	return n
}
