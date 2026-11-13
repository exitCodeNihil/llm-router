-- Paged key list in the console: newest first, with id as the tie-breaker.
CREATE INDEX api_keys_created ON api_keys (created_at DESC, id DESC);
