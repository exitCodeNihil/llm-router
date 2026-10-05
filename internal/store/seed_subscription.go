package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// SeedClaudeSubscription registers the pass-through provider that lets Claude
// Code use its own claude.ai subscription through the gateway, with one model,
// claude-*, that forwards whatever Claude model name the client asks for. New
// Claude models therefore work without a gateway update. It stores no
// credential at all — Claude Code brings its own token on every request — so
// it is safe to run by default in a fresh install. A no-op once the provider
// exists, so an admin's edits and deletions stick.
func (s *Store) SeedClaudeSubscription(ctx context.Context) error {
	const name = "anthropic-subscription"
	var exists bool
	if err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM providers WHERE name=$1)`, name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	pid := uuid.New()
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO providers (id, name, type, base_url, auth_mode, config)
		VALUES ($1, $2, 'openai_compatible', 'https://api.anthropic.com', 'oauth_passthrough', '{}')`,
		pid, name); err != nil {
		return fmt.Errorf("seed subscription provider: %w", err)
	}
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO model_deployments (id, provider_id, model_name, upstream_name, api_flavor)
		VALUES ($1, $2, 'claude-*', 'claude-*', 'anthropic')`, uuid.New(), pid); err != nil {
		return fmt.Errorf("seed subscription model: %w", err)
	}
	return s.BumpConfigVersion(ctx)
}
