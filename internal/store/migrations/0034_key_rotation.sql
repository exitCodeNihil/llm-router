-- A rotated key's previous hash, served until expires_at so clients can move
-- to the new secret without an outage. Several can be live at once when a key
-- is rotated again during a grace period.
CREATE TABLE api_key_old_hashes (
    key_hash   bytea PRIMARY KEY,
    key_id     uuid NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);
CREATE INDEX api_key_old_hashes_key ON api_key_old_hashes (key_id);
CREATE INDEX api_key_old_hashes_expires ON api_key_old_hashes (expires_at);
