-- Running spend per scope for the current month and for all time, kept next to
-- the per-day rows in spend_counters. Budget checks read one row per scope
-- from here instead of summing every day there has ever been.
CREATE TABLE spend_totals (
    period       text NOT NULL CHECK (period IN ('month','total')),
    period_start date NOT NULL,   -- first day of the month; 1970-01-01 for 'total'
    scope_type   text NOT NULL,
    scope_id     text NOT NULL,
    usd          numeric NOT NULL DEFAULT 0,
    tokens       bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (period, period_start, scope_type, scope_id)
);

-- Writers wait while the totals are built from the days already recorded.
LOCK TABLE spend_counters IN SHARE ROW EXCLUSIVE MODE;

INSERT INTO spend_totals (period, period_start, scope_type, scope_id, usd, tokens)
SELECT 'total', DATE '1970-01-01', scope_type, scope_id, sum(usd), sum(tokens)
FROM spend_counters GROUP BY scope_type, scope_id;

INSERT INTO spend_totals (period, period_start, scope_type, scope_id, usd, tokens)
SELECT 'month', date_trunc('month', now() AT TIME ZONE 'utc')::date, scope_type, scope_id, sum(usd), sum(tokens)
FROM spend_counters
WHERE period_start >= date_trunc('month', now() AT TIME ZONE 'utc')::date
GROUP BY scope_type, scope_id;

-- Today's rows, and later the days to prune, are found by date.
CREATE INDEX spend_counters_period ON spend_counters (period_start);
