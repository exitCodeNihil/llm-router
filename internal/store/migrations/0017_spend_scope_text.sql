-- The "member" spend scope is "<user_id>:<team_id>", which is not a uuid.
-- Widen scope_id so every scope kind fits and admit the new scope type;
-- the primary key is unchanged.
ALTER TABLE spend_counters ALTER COLUMN scope_id TYPE text USING scope_id::text;
ALTER TABLE spend_counters DROP CONSTRAINT IF EXISTS spend_counters_scope_type_check;
ALTER TABLE spend_counters ADD CONSTRAINT spend_counters_scope_type_check
    CHECK (scope_type IN ('key','user','team','member'));
