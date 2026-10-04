-- Google Cloud Vertex AI as a provider type: Gemini over its OpenAI-compatible
-- endpoint, Claude over the Anthropic rawPredict surface.
--   gcp_adc — Application Default Credentials (attached service account,
--             workload identity, or `gcloud auth application-default login`)
--   gcp_sa  — a service account JSON key, stored encrypted like any other secret
ALTER TABLE providers DROP CONSTRAINT providers_type_check;
ALTER TABLE providers ADD CONSTRAINT providers_type_check
    CHECK (type IN ('azure','openai_compatible','gcp_vertex'));

ALTER TABLE providers DROP CONSTRAINT providers_auth_mode_check;
ALTER TABLE providers ADD CONSTRAINT providers_auth_mode_check
    CHECK (auth_mode IN ('entra','api_key','bearer','none','oauth_passthrough','gcp_adc','gcp_sa'));
