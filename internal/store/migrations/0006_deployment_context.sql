-- Optional context-window size for a deployment, in tokens.
--
-- Purely informational: the gateway does not enforce it. The console uses it to
-- show how full a playground conversation is getting. Nullable because most
-- providers do not report a reliable context length, so an operator sets it
-- when they care.
ALTER TABLE model_deployments ADD COLUMN context_tokens integer;
