-- Anthropic gets its own provider type. Providers already pointing at its API
-- become that type: they keep working the same, with the same auth mode.
ALTER TABLE providers DROP CONSTRAINT providers_type_check;
ALTER TABLE providers ADD CONSTRAINT providers_type_check
    CHECK (type IN ('azure','openai_compatible','gcp_vertex','openrouter','anthropic'));
UPDATE providers SET type = 'anthropic'
    WHERE type = 'openai_compatible'
      AND regexp_replace(base_url, '/(v1/?)?$', '') = 'https://api.anthropic.com';
