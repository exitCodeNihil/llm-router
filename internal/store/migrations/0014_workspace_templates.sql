-- Workspace templates: an admin-curated list of images (with the Dockerfile
-- that produced them) that users pick from when creating a workspace. Shipped
-- rows are source='seed' and get refreshed on upgrade; an admin's edits flip a
-- row to 'admin' and are left alone from then on.
CREATE TABLE workspace_templates (
    id          uuid PRIMARY KEY,
    name        text UNIQUE NOT NULL,
    description text NOT NULL DEFAULT '',
    image       text NOT NULL,
    dockerfile  text NOT NULL DEFAULT '',
    source      text NOT NULL DEFAULT 'admin' CHECK (source IN ('seed','admin')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE workspaces ADD COLUMN template_id uuid REFERENCES workspace_templates(id) ON DELETE SET NULL;

-- Files a user wants in every workspace they own — ssh keys, .gitconfig,
-- .npmrc, cloud credentials. Written into $HOME on every start, encrypted at
-- rest like provider secrets.
CREATE TABLE workspace_user_files (
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    path       text NOT NULL,                 -- relative to $HOME
    mode       int  NOT NULL DEFAULT 420,     -- 0644; ssh material gets 0600
    data_enc   bytea NOT NULL,
    size       int  NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, path)
);
