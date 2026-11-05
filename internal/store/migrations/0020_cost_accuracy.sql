-- Cache writes cost more than plain input, so usage keeps them apart from the prompt
-- total, and a catalog price can rise for long prompts (Claude Haiku 5.5 over 100k tokens).
ALTER TABLE usage_events
    ADD COLUMN cache_write_5m_tokens int NOT NULL DEFAULT 0,
    ADD COLUMN cache_write_1h_tokens int NOT NULL DEFAULT 0;
ALTER TABLE price_catalog
    ADD COLUMN long_prompt_tokens int,
    ADD COLUMN long_prompt_multiplier numeric;
