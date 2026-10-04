-- What the traffic would have cost at list rates, kept apart from cost_usd.
--
-- Subscription (oauth_passthrough) traffic has no marginal cost, so cost_usd is
-- correctly 0 and unpriced. That leaves the tokens unvalued, which makes it
-- impossible to answer "is the subscription worth it". This column holds the
-- priced equivalent and is never added to spend, budgets or invoices.
ALTER TABLE usage_events ADD COLUMN notional_cost_usd numeric(18,10);
