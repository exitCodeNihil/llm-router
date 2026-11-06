-- Telemetry destinations: every place usage traces are exported to. The
-- capture policy in settings.telemetry still decides what may leave; each
-- destination then picks the teams and gateway groups it takes and whether it
-- takes content. kind has no CHECK so a new kind needs no migration.
CREATE TABLE telemetry_destinations (
    id                uuid PRIMARY KEY,
    name              text UNIQUE NOT NULL,
    kind              text NOT NULL,
    enabled           boolean NOT NULL DEFAULT true,
    endpoint          text NOT NULL DEFAULT '',
    traces_url        text NOT NULL DEFAULT '', -- overrides endpoint + /v1/traces
    config            jsonb NOT NULL DEFAULT '{}', -- public fields only
    secrets_enc       bytea, -- AES-GCM JSON object, LLMR_ENCRYPTION_KEY
    secret_names      text[] NOT NULL DEFAULT '{}',
    content_mode      text NOT NULL DEFAULT 'metadata_only' CHECK (content_mode IN ('metadata_only', 'content')),
    max_content_bytes int NOT NULL DEFAULT 65536 CHECK (max_content_bytes BETWEEN 1 AND 1048576),
    -- Values are team ids, and gateway group ids or _control_plane.
    team_scope        text NOT NULL DEFAULT 'all' CHECK (team_scope IN ('all', 'selected', 'except')),
    team_values       text[] NOT NULL DEFAULT '{}',
    group_scope       text NOT NULL DEFAULT 'all' CHECK (group_scope IN ('all', 'selected', 'except')),
    group_values      text[] NOT NULL DEFAULT '{}',
    -- Delivery status, written by the exporter at most every 30 s.
    last_success_at   timestamptz,
    last_error        text,
    last_error_at     timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
