-- Per-team, per-member budget: a member's share of the team's pool. NULL =
-- no per-member cap. Spend is tracked per (user, team) pair as scope type
-- "member" with scope_id "<user_id>:<team_id>".
ALTER TABLE team_members ADD COLUMN IF NOT EXISTS budget_usd numeric;
ALTER TABLE team_members ADD COLUMN IF NOT EXISTS budget_period text;
