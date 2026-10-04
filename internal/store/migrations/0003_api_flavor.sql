-- which protocol the upstream deployment speaks: OpenAI chat-completions or
-- Anthropic Messages (Claude models on Azure AI Foundry)
ALTER TABLE model_deployments ADD COLUMN api_flavor text NOT NULL DEFAULT 'openai'
    CHECK (api_flavor IN ('openai','anthropic'));
