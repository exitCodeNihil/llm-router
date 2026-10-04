-- Model policy on users and teams. NULL = every model; a list narrows what
-- any key of that user / team may call. Effective access is the intersection
-- of team, user and key.
ALTER TABLE users ADD COLUMN IF NOT EXISTS allowed_models text[];
ALTER TABLE teams ADD COLUMN IF NOT EXISTS allowed_models text[];
