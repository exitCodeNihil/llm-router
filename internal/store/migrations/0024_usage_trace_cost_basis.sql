-- trace_id links a request to its exported trace. inference_geo and speed keep
-- what Anthropic reported, which raises the price. cost_basis is the pricing
-- the cost was computed with: 1 is 0.3.0's; NULL is an earlier row, whose cost
-- can't be recomputed.
ALTER TABLE usage_events
    ADD COLUMN trace_id text,
    ADD COLUMN inference_geo text,
    ADD COLUMN speed text,
    ADD COLUMN cost_basis smallint;
