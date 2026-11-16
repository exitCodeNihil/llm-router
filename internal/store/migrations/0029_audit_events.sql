-- Administrative actions: who did what to which record, with what outcome.
-- Rows are only ever appended. Retention pruning (and erasure) set
-- llmr.audit_maintenance for their own transaction to get past the guard.
CREATE TABLE audit_events (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ts            timestamptz NOT NULL DEFAULT now(),
    actor_kind    text NOT NULL CHECK (actor_kind IN ('user', 'admin_token', 'anonymous')),
    actor_user_id uuid,
    actor_email   text,
    action        text NOT NULL,
    target_type   text,
    target_id     text,
    status        int NOT NULL,
    client_ip     text,
    detail        jsonb
);

CREATE INDEX audit_events_ts ON audit_events (ts);
CREATE INDEX audit_events_target ON audit_events (target_type, target_id);
CREATE INDEX audit_events_actor ON audit_events (actor_user_id, id);

CREATE FUNCTION audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF coalesce(current_setting('llmr.audit_maintenance', true), '') <> 'on' THEN
        RAISE EXCEPTION 'audit_events is append-only: % refused', TG_OP
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NULL;
END $$;

CREATE TRIGGER audit_events_append_only
    BEFORE UPDATE OR DELETE OR TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();
