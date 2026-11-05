// Package pricing seeds the price catalog and resolves cost per request.
package pricing

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/exitcodenihil/llm-router/internal/snapshot"
)

//go:embed azure-prices.json
var seedJSON []byte

type seedEntry struct {
	InputPer1M       float64  `json:"input_per_1m"`
	OutputPer1M      float64  `json:"output_per_1m"`
	CachedInputPer1M *float64 `json:"cached_input_per_1m"`
	LongPromptTokens *int     `json:"long_prompt_tokens"`
	LongPromptMult   *float64 `json:"long_prompt_multiplier"`
}

// Seed upserts the shipped catalog, skipping rows an admin has edited
// (source='admin' survives upgrades).
func Seed(ctx context.Context, pool *pgxpool.Pool) error {
	var entries map[string]seedEntry
	if err := json.Unmarshal(seedJSON, &entries); err != nil {
		return fmt.Errorf("parse embedded price catalog: %w", err)
	}
	for id, e := range entries {
		if _, err := pool.Exec(ctx, `
			INSERT INTO price_catalog (model_id, input_per_1m, output_per_1m, cached_input_per_1m,
				long_prompt_tokens, long_prompt_multiplier, source, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'seed', now())
			ON CONFLICT (model_id) DO UPDATE
			SET input_per_1m = EXCLUDED.input_per_1m,
			    output_per_1m = EXCLUDED.output_per_1m,
			    cached_input_per_1m = EXCLUDED.cached_input_per_1m,
			    long_prompt_tokens = EXCLUDED.long_prompt_tokens,
			    long_prompt_multiplier = EXCLUDED.long_prompt_multiplier,
			    updated_at = now()
			WHERE price_catalog.source = 'seed'`,
			id, e.InputPer1M, e.OutputPer1M, e.CachedInputPer1M, e.LongPromptTokens, e.LongPromptMult); err != nil {
			return fmt.Errorf("seed price %s: %w", id, err)
		}
	}
	return nil
}

// Tokens is what an upstream reported for one request. Prompt is every input
// token: Cached, Write5m and Write1h are parts of it.
type Tokens struct {
	Prompt, Completion, Cached, Write5m, Write1h int
	USInference, Fast                            bool
}

// Anthropic's pricing rules, applied on top of a model's rates. One figure fits
// every Claude model today; a model that differs needs its own rate here.
const (
	cacheWrite5mMultiplier = 1.25
	cacheWrite1hMultiplier = 2.0
	usInferenceMultiplier  = 1.1 // inference_geo: "us"
	fastModeMultiplier     = 2.0 // speed: "fast"
)

// Cost resolves the price for a deployment and computes request cost.
// Resolution: deployment custom price → catalog price → unpriced (cost 0).
func Cost(s *snapshot.Snapshot, d *snapshot.Deployment, t Tokens) (costUSD float64, unpriced bool) {
	// Pass-through traffic is billed by the upstream against the caller's own
	// plan (Claude Code on a subscription), so any dollar figure here is fiction:
	// record it unpriced and judge it on tokens instead. Notional gives the
	// priced equivalent for the same request, kept in its own column.
	if d.Provider != nil && d.Provider.AuthMode == "oauth_passthrough" {
		return 0, true
	}
	return price(s, d, t)
}

// Notional prices a request at list rates regardless of how it was billed, so
// subscription traffic can be valued without polluting spend. A deployment with
// no price of its own (the claude-* wildcard) uses the Anthropic catalog entry
// for the model asked for. Returns 0 when no price is known.
func Notional(s *snapshot.Snapshot, d *snapshot.Deployment, model string, t Tokens) float64 {
	if d == nil {
		return 0
	}
	if usd, unpriced := price(s, d, t); !unpriced {
		return usd
	}
	if p, ok := s.Prices["anthropic/"+model]; ok {
		return priceCost(p, t)
	}
	return 0
}

func price(s *snapshot.Snapshot, d *snapshot.Deployment, t Tokens) (costUSD float64, unpriced bool) {
	switch {
	case d.InputPer1M != nil && d.OutputPer1M != nil:
		p := snapshot.Price{InputPer1M: *d.InputPer1M, OutputPer1M: *d.OutputPer1M, CachedInputPer1M: d.CachedInputPer1M}
		return priceCost(p, t), false
	case d.CatalogModelID != "":
		p, ok := s.Prices[d.CatalogModelID]
		if !ok {
			return 0, true
		}
		return priceCost(p, t), false
	}
	return 0, true
}

func priceCost(p snapshot.Price, t Tokens) float64 {
	// The parts can't exceed the whole, whatever an upstream reports.
	part := func(n, left int) int { return max(0, min(n, left)) }
	cached := part(t.Cached, t.Prompt)
	write5m := part(t.Write5m, t.Prompt-cached)
	write1h := part(t.Write1h, t.Prompt-cached-write5m)
	fresh := t.Prompt - cached - write5m - write1h

	cachedRate := p.InputPer1M // no cached rate: reads bill at the input rate
	if p.CachedInputPer1M != nil {
		cachedRate = *p.CachedInputPer1M
	}
	usd := float64(fresh)*p.InputPer1M +
		float64(cached)*cachedRate +
		float64(write5m)*p.InputPer1M*cacheWrite5mMultiplier +
		float64(write1h)*p.InputPer1M*cacheWrite1hMultiplier +
		float64(t.Completion)*p.OutputPer1M
	if p.LongPromptTokens > 0 && t.Prompt > p.LongPromptTokens {
		usd *= p.LongPromptMultiplier
	}
	if t.USInference {
		usd *= usInferenceMultiplier
	}
	if t.Fast {
		usd *= fastModeMultiplier
	}
	return usd / 1e6
}
