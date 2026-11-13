-- What deleted users, keys and teams were called, so usage history keeps
-- naming them. label is the user's email, the key's or team's name; erasing a
-- user replaces it. For a key, user_id and team_id are its owners; for a user,
-- user_id is its own id.
CREATE TABLE deleted_entities (
    kind       text NOT NULL CHECK (kind IN ('user', 'key', 'team')),
    id         uuid NOT NULL,
    label      text NOT NULL,
    user_id    uuid,
    team_id    uuid,
    deleted_at timestamptz NOT NULL DEFAULT now(),
    deleted_by uuid,
    erased_at  timestamptz,
    PRIMARY KEY (kind, id)
);

CREATE INDEX deleted_entities_user ON deleted_entities (user_id);
