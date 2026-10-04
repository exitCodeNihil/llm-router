-- OpenRouter: an OpenAI-compatible aggregator whose /models answer carries
-- per-model pricing, so discovery can fill in custom prices instead of the
-- operator copying them by hand.
ALTER TABLE providers DROP CONSTRAINT providers_type_check;
ALTER TABLE providers ADD CONSTRAINT providers_type_check
    CHECK (type IN ('azure','openai_compatible','gcp_vertex','openrouter'));
