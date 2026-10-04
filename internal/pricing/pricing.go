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
			INSERT INTO price_catalog (model_id, input_per_1m, output_per_1m, cached_input_per_1m, source, updated_at)
			VALUES ($1, $2, $3, $4, 'seed', now())
			ON CONFLICT (model_id) DO UPDATE
			SET input_per_1m = EXCLUDED.input_per_1m,
			    output_per_1m = EXCLUDED.output_per_1m,
			    cached_input_per_1m = EXCLUDED.cached_input_per_1m,
			    updated_at = now()
			WHERE price_catalog.source = 'seed'`,
			id, e.InputPer1M, e.OutputPer1M, e.CachedInputPer1M); err != nil {
			return fmt.Errorf("seed price %s: %w", id, err)
		}
	}
	return nil
}

// Cost resolves the price for a deployment and computes request cost.
// Resolution: deployment custom price → catalog price → unpriced (cost 0).
func Cost(s *snapshot.Snapshot, d *snapshot.Deployment, promptTokens, completionTokens, cachedTokens int) (costUSD float64, unpriced bool) {
	// Pass-through traffic is billed by the upstream against the caller's own
	// plan (Claude Code on a subscription), so any dollar figure here is fiction:
	// record it unpriced and judge it on tokens instead. Notional gives the
	// priced equivalent for the same request, kept in its own column.
	if d.Provider != nil && d.Provider.AuthMode == "oauth_passthrough" {
		return 0, true
	}
	return price(s, d, promptTokens, completionTokens, cachedTokens)
}

// Notional prices a request at list rates regardless of how it was billed, so
// subscription traffic can be valued without polluting spend. Returns 0 when no
// price is configured for the deployment.
func Notional(s *snapshot.Snapshot, d *snapshot.Deployment, promptTokens, completionTokens, cachedTokens int) float64 {
	if d == nil {
		return 0
	}
	usd, unpriced := price(s, d, promptTokens, completionTokens, cachedTokens)
	if unpriced {
		return 0
	}
	return usd
}

func price(s *snapshot.Snapshot, d *snapshot.Deployment, promptTokens, completionTokens, cachedTokens int) (costUSD float64, unpriced bool) {
	var in, out float64
	var cached *float64

	switch {
	case d.InputPer1M != nil && d.OutputPer1M != nil:
		in, out, cached = *d.InputPer1M, *d.OutputPer1M, d.CachedInputPer1M
	case d.CatalogModelID != "":
		p, ok := s.Prices[d.CatalogModelID]
		if !ok {
			return 0, true
		}
		in, out, cached = p.InputPer1M, p.OutputPer1M, p.CachedInputPer1M
	default:
		return 0, true
	}

	// cached tokens are a subset of prompt tokens; bill them at the cached rate
	billedPrompt := promptTokens
	var cachedCost float64
	if cached != nil && cachedTokens > 0 {
		if cachedTokens > promptTokens {
			cachedTokens = promptTokens
		}
		billedPrompt = promptTokens - cachedTokens
		cachedCost = float64(cachedTokens) * *cached / 1e6
	}
	return float64(billedPrompt)*in/1e6 + float64(completionTokens)*out/1e6 + cachedCost, false
}
