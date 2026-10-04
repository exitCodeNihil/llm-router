-- Developer workspaces: a container per workspace, owned by one user.
--
-- State kept here is only what the control plane cannot ask the runtime for:
-- ownership, the image it was created from, and the API key minted for the
-- agent running inside it. Container liveness is read from the runtime, not
-- cached here, so a container that dies out from under us is never reported as
-- running. The workspace's files live on a named volume, not in Postgres.
CREATE TABLE workspaces (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         text NOT NULL,
    image        text NOT NULL,
    container_id text NOT NULL DEFAULT '',
    -- Last known state; authoritative status comes from the runtime.
    status       text NOT NULL DEFAULT 'stopped',
    -- The agent inside the workspace authenticates to the gateway with this
    -- key, so its spend is metered and budget-capped like any other client.
    api_key_id   uuid REFERENCES api_keys(id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX workspaces_user ON workspaces (user_id, created_at DESC);
