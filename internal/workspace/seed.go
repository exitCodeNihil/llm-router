package workspace

import (
	"context"
	"embed"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed templates/*.Dockerfile
var templateFS embed.FS

// Template is a workspace image users can pick from, with the Dockerfile
// that produces it so an admin can read, edit and rebuild it in the console.
type Template struct {
	Name, Description, Image, Dockerfile string
}

var seedTemplates = []Template{
	{"base", "Git, a shell and the pi agent. Start here for anything without a toolchain.", "llmr-workspace-base", ""},
	{"python", "Python 3.13 with pip. Packages cache under $HOME on the volume.", "llmr-workspace-python", ""},
	{"node", "Node 22 with npm, pnpm and yarn (corepack) and TypeScript.", "llmr-workspace-node", ""},
	{"go", "Go with golangci-lint, gopls and delve. Module and build caches persist.", "llmr-workspace-go", ""},
	{"java", "Temurin 21 JDK with Maven and Gradle. ~/.m2 and ~/.gradle persist.", "llmr-workspace-java", ""},
	{"infra", "OpenTofu, Terragrunt, TFLint, kubectl and Helm. Provider plugin cache persists.", "llmr-workspace-infra", ""},
}

// SeedTemplates upserts the shipped templates, leaving rows an admin edited
// (source='admin') untouched — the same contract as the price catalog.
func SeedTemplates(ctx context.Context, pool *pgxpool.Pool) error {
	for _, t := range seedTemplates {
		df, err := templateFS.ReadFile("templates/" + t.Name + ".Dockerfile")
		if err != nil {
			return fmt.Errorf("template %s: %w", t.Name, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO workspace_templates (id, name, description, image, dockerfile, source)
			VALUES ($1, $2, $3, $4, $5, 'seed')
			ON CONFLICT (name) DO UPDATE
			SET description = EXCLUDED.description, image = EXCLUDED.image,
			    dockerfile = EXCLUDED.dockerfile, updated_at = now()
			WHERE workspace_templates.source = 'seed'
			  AND workspace_templates.dockerfile IS DISTINCT FROM EXCLUDED.dockerfile`,
			uuid.New(), t.Name, t.Description, t.Image, strings.TrimSpace(string(df))+"\n"); err != nil {
			return fmt.Errorf("seed template %s: %w", t.Name, err)
		}
	}
	return nil
}
