-- When the group's join token was last replaced. A gateway that joined before
-- then holds the old token: it keeps its session, and cannot join again.
ALTER TABLE gateway_groups ADD COLUMN token_rotated_at timestamptz;
