-- The caller's address, recorded only when LLMR_USAGE_CLIENT_IP is on: it is
-- personal data. Rollups leave it out.
ALTER TABLE usage_events ADD COLUMN client_ip inet;
