-- Anthropic workload identity federation: the gateway exchanges its own identity
-- token for short-lived Anthropic tokens, so the provider stores no secret.
ALTER TABLE providers DROP CONSTRAINT providers_auth_mode_check;
ALTER TABLE providers ADD CONSTRAINT providers_auth_mode_check
    CHECK (auth_mode IN ('entra','api_key','bearer','none','oauth_passthrough','gcp_adc','gcp_sa','anthropic_wif'));
