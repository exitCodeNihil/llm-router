-- Gateway groups: data-plane gateways join with the group's token, take their
-- config from the control plane and sync usage and limit counters with it.
-- A group decides who may use its gateways, which region they prefer and what
-- telemetry may leave them; limits and budgets stay on keys, users and teams.
CREATE TABLE gateway_groups (
    id              uuid PRIMARY KEY,
    name            text UNIQUE NOT NULL,
    region          text NOT NULL DEFAULT '',
    allowed_teams   text[], -- NULL or empty: every team
    data_rule       text NOT NULL DEFAULT 'inherit' CHECK (data_rule IN ('inherit', 'no_content', 'no_export')),
    join_token_hash bytea UNIQUE NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- One row per running gateway process. last_batch makes usage uploads
-- idempotent: a gateway resends a batch until it is acknowledged.
CREATE TABLE gateway_instances (
    id           uuid PRIMARY KEY,
    group_id     uuid NOT NULL REFERENCES gateway_groups(id) ON DELETE CASCADE,
    name         text NOT NULL DEFAULT '',
    version      text NOT NULL DEFAULT '',
    last_batch   bigint NOT NULL DEFAULT 0,
    joined_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz
);
CREATE INDEX gateway_instances_group ON gateway_instances (group_id);

-- region: gateways in a group with the same region try this deployment first.
-- timeout_ms: how long to wait for the upstream's response headers before
-- failing over; NULL is the gateway default.
ALTER TABLE model_deployments ADD COLUMN region text NOT NULL DEFAULT '';
ALTER TABLE model_deployments ADD COLUMN timeout_ms int CHECK (timeout_ms > 0);
