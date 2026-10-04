/**
 * Client-side cost estimate for a playground turn.
 *
 * Mirrors internal/pricing.Cost exactly, including the cached-token rule
 * (cached tokens are a subset of prompt tokens and are billed at the cached
 * rate). Kept in step with that function — if the server's pricing rules
 * change, change this too.
 *
 * The server remains the source of truth: what is recorded in usage_events is
 * what gets billed. This exists so a turn can show its cost immediately rather
 * than waiting for the async usage writer.
 */
import type { Deployment, Price } from "./types";

export interface TokenCounts {
  prompt: number;
  completion: number;
  cached?: number;
}

export interface CostEstimate {
  usd: number;
  /** True when no price applies, so the request would be recorded unpriced. */
  unpriced: boolean;
}

/**
 * Resolve the rates for a deployment: a custom per-deployment override wins,
 * then the catalog entry, otherwise the request is unpriced.
 */
function rates(
  deployment: Deployment | undefined,
  prices: Map<string, Price>,
): { input: number; output: number; cached?: number } | null {
  if (!deployment) return null;
  if (deployment.input_per_1m != null && deployment.output_per_1m != null) {
    return {
      input: deployment.input_per_1m,
      output: deployment.output_per_1m,
      cached: deployment.cached_input_per_1m ?? undefined,
    };
  }
  if (deployment.catalog_model_id) {
    const p = prices.get(deployment.catalog_model_id);
    if (!p) return null;
    return { input: p.input_per_1m, output: p.output_per_1m, cached: p.cached_input_per_1m ?? undefined };
  }
  return null;
}

export function estimateCost(
  deployment: Deployment | undefined,
  prices: Map<string, Price>,
  counts: TokenCounts,
): CostEstimate {
  const r = rates(deployment, prices);
  if (!r) return { usd: 0, unpriced: true };

  let billedPrompt = counts.prompt;
  let cachedCost = 0;
  if (r.cached != null && (counts.cached ?? 0) > 0) {
    const cached = Math.min(counts.cached ?? 0, counts.prompt);
    billedPrompt = counts.prompt - cached;
    cachedCost = (cached * r.cached) / 1e6;
  }

  return {
    usd: (billedPrompt * r.input) / 1e6 + (counts.completion * r.output) / 1e6 + cachedCost,
    unpriced: false,
  };
}

/**
 * Rough token count for text that has not been sent yet, used only for the
 * context meter before the first response. ~4 characters per token is the
 * usual English approximation; it is not exact for code or CJK.
 *
 * ponytail: swap for a real tokenizer (gpt-tokenizer / tiktoken) if the meter
 * ever needs to be accurate rather than indicative.
 */
export const estimateTokens = (text: string): number => Math.ceil(text.length / 4);
