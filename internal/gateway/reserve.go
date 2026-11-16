package gateway

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/exitcodenihil/llm-router/internal/auth"
	"github.com/exitcodenihil/llm-router/internal/pricing"
	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

// A request holds its expected cost against its budgets while it runs
// (limits.Checker.Reserve), so requests in parallel can't all pass on the
// same remaining dollars. The estimate is of what a request like it usually
// costs, not the most it could: agents ask for tens of thousands of output
// tokens and use a fraction, and holding the maximum would turn requests
// away from budgets that are nowhere near spent.

const (
	// Output tokens assumed for a model until enough requests have been seen.
	defaultOutputTokens = 1024
	outputSamples       = 20
)

// outputStats is the running mean of completion tokens per model.
type outputStats struct {
	mu sync.Mutex
	m  map[string]*[2]float64 // requests, tokens
}

func (o *outputStats) add(model string, tokens int) {
	if tokens <= 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.m == nil {
		o.m = map[string]*[2]float64{}
	}
	s := o.m[model]
	if s == nil {
		if len(o.m) >= 10000 { // model names come from callers
			return
		}
		s = &[2]float64{}
		o.m[model] = s
	}
	// Forget slowly, so the mean follows how the model is used now.
	if s[0] >= 1000 {
		s[0], s[1] = s[0]/2, s[1]/2
	}
	s[0]++
	s[1] += float64(tokens)
}

// expected is the output tokens a request for model is held for: twice the
// mean seen so far, or what it asked for if that is less.
func (o *outputStats) expected(model string, maxTokens int) int {
	out := defaultOutputTokens
	o.mu.Lock()
	if s := o.m[model]; s != nil && s[0] >= outputSamples {
		out = int(2 * s[1] / s[0])
	}
	o.mu.Unlock()
	if maxTokens > 0 && maxTokens < out {
		out = maxTokens
	}
	return out
}

// textTokens estimates a request body's input tokens at four bytes each,
// leaving out encoded images and documents: a screenshot is megabytes of
// base64 and a few hundred tokens.
func textTokens(raw []byte) int {
	n := len(raw)
	for i := 0; i < len(raw); i++ {
		if raw[i] != '"' {
			continue
		}
		start := i + 1
		for i++; i < len(raw) && raw[i] != '"'; i++ {
			if raw[i] == '\\' {
				i++
			}
		}
		if s := raw[start:min(i, len(raw))]; len(s) > 4096 && !strings.ContainsAny(string(s[:256]), " \n\\") {
			n -= len(s)
		}
	}
	return n / 4
}

// maxTokensOf reads the output cap from a chat or Messages body.
func maxTokensOf(fields map[string]json.RawMessage) int {
	for _, k := range []string{"max_completion_tokens", "max_tokens"} {
		var n int
		if v, ok := fields[k]; ok && json.Unmarshal(v, &n) == nil && n > 0 {
			return n
		}
	}
	return 0
}

// reserve holds the request's expected cost against the caller's budgets. ok
// false means a budget is taken up by requests in progress; release is always
// safe to call.
func (g *Server) reserve(snap *snapshot.Snapshot, id *auth.Identity, d *snapshot.Deployment, model string,
	raw []byte, maxTokens int, noOutput bool) (release func(), ok bool, msg string) {
	if g.Limits == nil {
		return func() {}, true, ""
	}
	t := pricing.Tokens{Prompt: textTokens(raw)}
	if !noOutput {
		t.Completion = g.outputs.expected(model, maxTokens)
	}
	usd, unpriced := pricing.Cost(snap, d, t)
	if unpriced {
		usd = 0 // billed elsewhere, or not at all: nothing to hold
	}
	return g.Limits.Reserve(id, usd)
}
