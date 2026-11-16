-- Hourly and daily rollups of usage_events, written with the events. Dashboards
-- read these, so raw events can be dropped after a retention period.
-- Needs Postgres 15 (UNIQUE NULLS NOT DISTINCT).

-- Latency histograms: 30 counts, for the ranges split at these milliseconds
-- (usage.histBounds). Percentiles are read from the summed counts.
CREATE FUNCTION llmr_hist_slot(ms integer) RETURNS integer
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT width_bucket(ms, ARRAY[10,15,25,35,50,75,100,150,250,350,500,750,1000,1500,2500,3500,5000,7500,10000,15000,25000,35000,50000,75000,100000,150000,250000,350000,500000])
$$;

-- A histogram with n in one slot (0 to 29) and zero elsewhere.
CREATE FUNCTION llmr_hist_at(slot integer, n bigint) RETURNS bigint[]
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT CASE WHEN slot IS NULL THEN NULL ELSE
        array_fill(0::bigint, ARRAY[slot]) || n || array_fill(0::bigint, ARRAY[29 - slot]) END
$$;

CREATE FUNCTION llmr_hist(ms integer) RETURNS bigint[]
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT llmr_hist_at(llmr_hist_slot(ms), 1)
$$;

CREATE FUNCTION llmr_hist_add(a bigint[], b bigint[]) RETURNS bigint[]
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT CASE WHEN a IS NULL THEN b WHEN b IS NULL THEN a ELSE (
        SELECT array_agg(x + y ORDER BY i) FROM unnest(a, b) WITH ORDINALITY AS t(x, y, i)) END
$$;

CREATE AGGREGATE llmr_hist_sum(bigint[]) (
    SFUNC = llmr_hist_add, STYPE = bigint[], COMBINEFUNC = llmr_hist_add, PARALLEL = SAFE
);

CREATE TABLE usage_hourly (
    bucket            timestamptz NOT NULL,   -- start of the hour, UTC
    team_id           uuid,
    user_id           uuid,
    api_key_id        uuid,
    model_name        text NOT NULL,
    provider_id       uuid,
    deployment_id     uuid,
    gateway_id        uuid,
    status_code       int NOT NULL,
    error_code        text,
    stream            boolean NOT NULL,
    unpriced          boolean NOT NULL,
    failover          boolean NOT NULL,       -- took more than one attempt
    requests          bigint NOT NULL DEFAULT 0,
    prompt_tokens     bigint NOT NULL DEFAULT 0,
    completion_tokens bigint NOT NULL DEFAULT 0,
    cached_tokens     bigint NOT NULL DEFAULT 0,
    cost_usd          numeric NOT NULL DEFAULT 0,
    notional_cost_usd numeric NOT NULL DEFAULT 0,
    latency_hist      bigint[],
    ttft_hist         bigint[],
    UNIQUE NULLS NOT DISTINCT (bucket, team_id, user_id, api_key_id, model_name, provider_id,
        deployment_id, gateway_id, status_code, error_code, stream, unpriced, failover)
) PARTITION BY RANGE (bucket);

-- Same shape, one row per UTC day.
CREATE TABLE usage_daily (LIKE usage_hourly INCLUDING DEFAULTS,
    UNIQUE NULLS NOT DISTINCT (bucket, team_id, user_id, api_key_id, model_name, provider_id,
        deployment_id, gateway_id, status_code, error_code, stream, unpriced, failover)
) PARTITION BY RANGE (bucket);

CREATE INDEX usage_hourly_team ON usage_hourly (team_id, bucket);
CREATE INDEX usage_hourly_user ON usage_hourly (user_id, bucket);
CREATE INDEX usage_hourly_key  ON usage_hourly (api_key_id, bucket);
CREATE INDEX usage_daily_team  ON usage_daily (team_id, bucket);
CREATE INDEX usage_daily_user  ON usage_daily (user_id, bucket);
CREATE INDEX usage_daily_key   ON usage_daily (api_key_id, bucket);

-- Writers wait while the rollups are built from the events already stored.
LOCK TABLE usage_events IN SHARE ROW EXCLUSIVE MODE;

-- Monthly partitions from the oldest event to next month. Hourly rollups are
-- kept for three months, so older months get none.
DO $$
DECLARE
    cur  timestamptz := date_trunc('month', now(), 'UTC');
    m    timestamptz;
    name text;
BEGIN
    SELECT least(date_trunc('month', min(ts), 'UTC'), cur - interval '1 month') INTO m FROM usage_events;
    m := coalesce(m, cur - interval '1 month');
    WHILE m < cur + interval '2 months' LOOP
        name := to_char(m AT TIME ZONE 'UTC', 'YYYY_MM');
        EXECUTE format('CREATE TABLE usage_daily_%s PARTITION OF usage_daily FOR VALUES FROM (%L) TO (%L)',
            name, m, m + interval '1 month');
        IF m >= cur - interval '3 months' THEN
            EXECUTE format('CREATE TABLE usage_hourly_%s PARTITION OF usage_hourly FOR VALUES FROM (%L) TO (%L)',
                name, m, m + interval '1 month');
        END IF;
        m := m + interval '1 month';
    END LOOP;
END $$;

-- Events are grouped by latency range first, so a histogram is built per
-- group of events and not per event.
INSERT INTO usage_daily
SELECT bucket, team_id, user_id, api_key_id, model_name, provider_id, deployment_id, gateway_id,
       status_code, error_code, stream, unpriced, failover,
       sum(n), sum(prompt), sum(completion), sum(cached), sum(cost), sum(notional),
       llmr_hist_sum(llmr_hist_at(latency, n)), llmr_hist_sum(llmr_hist_at(ttft, n))
FROM (
    SELECT date_trunc('day', ts, 'UTC') AS bucket, team_id, user_id, api_key_id, model_name, provider_id,
           deployment_id, gateway_id, status_code, error_code, stream, unpriced,
           coalesce(attempts, 0) > 1 AS failover,
           llmr_hist_slot(latency_ms) AS latency, llmr_hist_slot(ttft_ms) AS ttft,
           count(*) AS n, sum(prompt_tokens) AS prompt, sum(completion_tokens) AS completion,
           sum(cached_tokens) AS cached, sum(cost_usd) AS cost, coalesce(sum(notional_cost_usd), 0) AS notional
    FROM usage_events
    GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15
) by_latency
GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13;

INSERT INTO usage_hourly
SELECT bucket, team_id, user_id, api_key_id, model_name, provider_id, deployment_id, gateway_id,
       status_code, error_code, stream, unpriced, failover,
       sum(n), sum(prompt), sum(completion), sum(cached), sum(cost), sum(notional),
       llmr_hist_sum(llmr_hist_at(latency, n)), llmr_hist_sum(llmr_hist_at(ttft, n))
FROM (
    SELECT date_trunc('hour', ts, 'UTC') AS bucket, team_id, user_id, api_key_id, model_name, provider_id,
           deployment_id, gateway_id, status_code, error_code, stream, unpriced,
           coalesce(attempts, 0) > 1 AS failover,
           llmr_hist_slot(latency_ms) AS latency, llmr_hist_slot(ttft_ms) AS ttft,
           count(*) AS n, sum(prompt_tokens) AS prompt, sum(completion_tokens) AS completion,
           sum(cached_tokens) AS cached, sum(cost_usd) AS cost, coalesce(sum(notional_cost_usd), 0) AS notional
    FROM usage_events
    WHERE ts >= date_trunc('month', now(), 'UTC') - interval '3 months'
    GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15
) by_latency
GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13;
