-- Providers that forward the caller's own Authorization header upstream instead
-- of substituting a stored credential. Claude Code presents the subscription
-- OAuth token it already holds, so the gateway stores no credential at all.
ALTER TABLE providers DROP CONSTRAINT providers_auth_mode_check;
ALTER TABLE providers ADD CONSTRAINT providers_auth_mode_check
    CHECK (auth_mode IN ('entra','api_key','bearer','none','oauth_passthrough'));
