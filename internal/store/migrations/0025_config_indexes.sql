-- Lookups by user and by team that had no index. team_members' primary key
-- starts with team_id, and every console request asks for a user's teams.
CREATE INDEX team_members_user ON team_members (user_id);
CREATE INDEX api_keys_user ON api_keys (user_id);
CREATE INDEX api_keys_team ON api_keys (team_id);
-- Deleting a key clears it from its workspace.
CREATE INDEX workspaces_api_key ON workspaces (api_key_id);
