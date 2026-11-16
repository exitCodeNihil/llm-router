-- Console sessions stay stateless HMAC cookies. Revocation is two checks on
-- the per-request user lookup: a cookie issued at a lower epoch than the
-- user's is dead (sign out everywhere, password change, disable), and a
-- cookie whose id is listed here was signed out. Rows go once the cookie
-- would have expired anyway.
ALTER TABLE users ADD COLUMN session_epoch integer NOT NULL DEFAULT 0;

CREATE TABLE revoked_sessions (
    sid        text PRIMARY KEY,
    expires_at timestamptz NOT NULL
);
CREATE INDEX revoked_sessions_expires ON revoked_sessions (expires_at);
