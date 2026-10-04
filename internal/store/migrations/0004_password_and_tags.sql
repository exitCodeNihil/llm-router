-- local password authentication (nullable: SSO-only users have no password)
ALTER TABLE users ADD COLUMN password_hash bytea;

-- free-form tags for scoping (telemetry export rules, future routing rules)
ALTER TABLE users ADD COLUMN tags text[];
ALTER TABLE teams ADD COLUMN tags text[];
ALTER TABLE api_keys ADD COLUMN tags text[];
