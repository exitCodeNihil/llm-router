-- Cached-input rate for hand-priced deployments.
--
-- price_catalog has had cached_input_per_1m since 0001, but a deployment priced
-- by hand had no way to express it, so every cache read was billed at the full
-- input rate. On cache-heavy traffic (Claude Code sends ~99% of its prompt from
-- cache) that overstates the figure by roughly 7x. Nullable: leaving it unset
-- keeps the old behaviour, which is correct for upstreams that do not discount
-- cache reads.
ALTER TABLE model_deployments ADD COLUMN cached_input_per_1m numeric;
