CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE users (
    id            uuid PRIMARY KEY,
    email         citext UNIQUE NOT NULL,
    name          text NOT NULL DEFAULT '',
    role          text NOT NULL DEFAULT 'member' CHECK (role IN ('admin','member')),
    budget_usd    numeric,
    budget_period text CHECK (budget_period IN ('daily','monthly','total')),
    rpm_limit     int,
    tpm_limit     int,
    disabled      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE teams (
    id            uuid PRIMARY KEY,
    name          text UNIQUE NOT NULL,
    budget_usd    numeric,
    budget_period text CHECK (budget_period IN ('daily','monthly','total')),
    rpm_limit     int,
    tpm_limit     int,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE team_members (
    team_id uuid NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role    text NOT NULL DEFAULT 'member' CHECK (role IN ('admin','member')),
    PRIMARY KEY (team_id, user_id)
);

CREATE TABLE api_keys (
    id            uuid PRIMARY KEY,
    key_hash      bytea UNIQUE NOT NULL,
    key_prefix    text NOT NULL,
    name          text NOT NULL DEFAULT '',
    user_id       uuid REFERENCES users(id) ON DELETE CASCADE,
    team_id       uuid REFERENCES teams(id) ON DELETE CASCADE,
    allowed_models text[],
    budget_usd    numeric,
    budget_period text CHECK (budget_period IN ('daily','monthly','total')),
    rpm_limit     int,
    tpm_limit     int,
    expires_at    timestamptz,
    disabled      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (user_id IS NOT NULL OR team_id IS NOT NULL)
);

CREATE TABLE providers (
    id          uuid PRIMARY KEY,
    name        text UNIQUE NOT NULL,
    type        text NOT NULL CHECK (type IN ('azure','openai_compatible')),
    base_url    text NOT NULL,
    auth_mode   text NOT NULL CHECK (auth_mode IN ('entra','api_key','bearer','none')),
    api_key_enc bytea,
    config      jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE model_deployments (
    id               uuid PRIMARY KEY,
    provider_id      uuid NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    model_name       text NOT NULL,
    upstream_name    text NOT NULL,
    priority         int NOT NULL DEFAULT 0,
    catalog_model_id text,
    input_per_1m     numeric,
    output_per_1m    numeric,
    enabled          boolean NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider_id, upstream_name)
);

CREATE TABLE price_catalog (
    model_id            text PRIMARY KEY,
    input_per_1m        numeric NOT NULL,
    output_per_1m       numeric NOT NULL,
    cached_input_per_1m numeric,
    source              text NOT NULL DEFAULT 'seed' CHECK (source IN ('seed','admin')),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE usage_events (
    id                bigint GENERATED ALWAYS AS IDENTITY,
    ts                timestamptz NOT NULL,
    request_id        text NOT NULL,
    api_key_id        uuid,
    user_id           uuid,
    team_id           uuid,
    model_name        text NOT NULL,
    deployment_id     uuid,
    provider_id       uuid,
    prompt_tokens     int NOT NULL DEFAULT 0,
    completion_tokens int NOT NULL DEFAULT 0,
    cached_tokens     int NOT NULL DEFAULT 0,
    cost_usd          numeric NOT NULL DEFAULT 0,
    unpriced          boolean NOT NULL DEFAULT false,
    latency_ms        int NOT NULL DEFAULT 0,
    status_code       int NOT NULL DEFAULT 0,
    stream            boolean NOT NULL DEFAULT false,
    edge_node_id      uuid,
    PRIMARY KEY (id, ts)
) PARTITION BY RANGE (ts);

CREATE TABLE spend_counters (
    scope_type   text NOT NULL CHECK (scope_type IN ('key','user','team')),
    scope_id     uuid NOT NULL,
    period_start date NOT NULL,
    usd          numeric NOT NULL DEFAULT 0,
    tokens       bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (scope_type, scope_id, period_start)
);

CREATE TABLE token_issuers (
    id            uuid PRIMARY KEY,
    type          text NOT NULL CHECK (type IN ('entra','gcp')),
    issuer_url    text NOT NULL,
    audience      text NOT NULL,
    claim_mapping jsonb NOT NULL DEFAULT '{}',
    enabled       boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE edge_nodes (
    id                uuid PRIMARY KEY,
    name              text UNIQUE NOT NULL,
    token_hash        bytea UNIQUE NOT NULL,
    last_seen_at      timestamptz,
    last_seen_version bigint,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE settings (
    key   text PRIMARY KEY,
    value jsonb NOT NULL
);

INSERT INTO settings (key, value) VALUES ('config_version', '1');
