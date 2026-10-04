-- Dimensions the gateway already knew but discarded, which analytics needs to
-- answer "why did this fail" and "how often does failover fire".
--
-- attempts   1 when the first-choice deployment answered, higher after failover.
-- error_code the gateway's own reason (model_not_found, rate_limit_exceeded,
--            budget_exceeded, …). status_code alone collapses very different
--            failures into one number.
-- ttft_ms    time to first streamed token. For streaming clients this matters
--            more than total latency, which is dominated by response length.
ALTER TABLE usage_events ADD COLUMN attempts   integer;
ALTER TABLE usage_events ADD COLUMN error_code text;
ALTER TABLE usage_events ADD COLUMN ttft_ms    integer;
