-- Chat playground: per-user saved conversations.
-- ponytail: one JSONB message array per chat (playground scale), not a
-- messages table; revisit if chats ever need search or per-message metadata.
CREATE TABLE chats (
    id            uuid PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title         text NOT NULL DEFAULT 'New chat',
    model         text NOT NULL DEFAULT '',
    system_prompt text NOT NULL DEFAULT '',
    settings      jsonb NOT NULL DEFAULT '{}'::jsonb,
    messages      jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX chats_user_updated ON chats (user_id, updated_at DESC);
