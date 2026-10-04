# llm-router — development and build tasks.
#
#   make dev     gateway with live reload + console with HMR
#   make build   single binary with the console embedded
#   make check   everything CI runs
#
# Every variable below can be overridden on the command line:
#   make dev PORT=9000 LLMR_DATABASE_URL=postgres://…

BINARY   ?= llmrouter
CMD      ?= ./cmd/llmrouter
WEB      ?= web
PORT     ?= 8080
WEB_PORT ?= 5173

# ── Dev environment ──────────────────────────────────────────────────────────
# Sensible local defaults; export so both air and the gateway inherit them.
LLMR_DATABASE_URL   ?= postgres://llmrouter:llmrouter@localhost:5433/llmrouter
LLMR_ADMIN_TOKEN    ?= dev-admin-token
LLMR_ENCRYPTION_KEY ?= dev-encryption-key
LLMR_LISTEN         := :$(PORT)
# The workspace IDE gets its own origin, as in production.
LLMR_IDE_LISTEN     ?= :8081
LLMR_IDE_ORIGIN     ?= http://localhost:8081
export LLMR_DATABASE_URL LLMR_ADMIN_TOKEN LLMR_ENCRYPTION_KEY LLMR_LISTEN LLMR_IDE_LISTEN LLMR_IDE_ORIGIN

# Vite proxies API calls here; keep it in step with PORT.
export LLMR_DEV_BACKEND := http://localhost:$(PORT)

GOBIN     := $(shell go env GOPATH)/bin
AIR       := $(GOBIN)/air
NODE_DEPS := $(WEB)/node_modules/.package-lock.json

# ── Containers ───────────────────────────────────────────────────────────────
# Prefer podman, fall back to docker. Override with ENGINE=docker.
ENGINE     ?= $(shell command -v podman >/dev/null 2>&1 && echo podman || echo docker)
COMPOSE    ?= $(ENGINE) compose
DEV_STACK  := deploy/docker-compose.dev.yml
IMAGE      ?= llm-router:dev
DB_NAME    ?= llmrouter
DB_SERVICE ?= postgres

GO_SOURCES := $(shell find . -name '*.go' -not -path './web/node_modules/*' 2>/dev/null)

.DEFAULT_GOAL := help
.PHONY: help dev dev-api dev-web build web theme run test test-go test-web \
        check fmt fmt-check vet tidy audit verify verify-chat demo-gifs tools clean distclean \
        deps db-url stack stack-down stack-logs db-wait db-reset db-shell image image-run

# ── Help ─────────────────────────────────────────────────────────────────────

help: ## Show this help
	@printf "\033[1mllm-router\033[0m — make targets\n\n"
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@printf "\n  Gateway  http://localhost:$(PORT)\n"
	@printf "  Console  http://localhost:$(WEB_PORT)   (dev, hot reload)\n\n"

# ── Development ──────────────────────────────────────────────────────────────

dev: $(AIR) $(NODE_DEPS) db-wait ## Gateway (live reload) + console (HMR)
	@if lsof -iTCP:$(PORT) -sTCP:LISTEN -t >/dev/null 2>&1; then \
	  echo ""; \
	  echo "  ✗ port $(PORT) is already in use."; \
	  echo "    Free it with:  kill \$$(lsof -iTCP:$(PORT) -sTCP:LISTEN -t)"; \
	  echo "    Or pick another: make dev PORT=9000"; \
	  echo ""; \
	  exit 1; \
	fi
	@printf "\n  \033[1mgateway\033[0m  http://localhost:$(PORT)      (air: rebuilds on .go changes)\n"
	@printf "  \033[1mconsole\033[0m  http://localhost:$(WEB_PORT)      (vite: HMR on .tsx changes)\n"
	@printf "  \033[2mdb %s\033[0m\n\n" "$(LLMR_DATABASE_URL)"
	@trap 'kill 0' EXIT INT TERM; \
	  $(AIR) -c .air.toml & \
	  npm --prefix $(WEB) run dev --silent & \
	  wait

dev-api: $(AIR) ## Gateway only, with live reload
	@$(AIR) -c .air.toml

dev-web: $(NODE_DEPS) ## Console only, with HMR
	@npm --prefix $(WEB) run dev

# ── Build ────────────────────────────────────────────────────────────────────

build: web ## Build the single binary (console embedded)
	@go build -ldflags="-s -w" -o $(BINARY) $(CMD)
	@printf "  built \033[1m./$(BINARY)\033[0m (%s)\n" "$$(du -h $(BINARY) | cut -f1)"

web: $(NODE_DEPS) ## Build the console into web/dist
	@npm --prefix $(WEB) run build

theme: $(NODE_DEPS) ## Recompile the Astryx theme from web/src/theme.ts
	@npm --prefix $(WEB) run theme

run: build ## Build, then run the binary
	@./$(BINARY)

# ── Quality ──────────────────────────────────────────────────────────────────

check: vet test ## Everything CI runs (vet, tests, console typecheck)

test: test-go test-web ## Go tests + console typecheck

test-go: ## Go tests
	@go test ./...

test-web: $(NODE_DEPS) ## Console typecheck
	@npm --prefix $(WEB) run typecheck

# Opt-in: the tree has pre-existing gofmt drift, so this is not part of `check`.
fmt: ## Format Go sources (not run by check)
	@gofmt -l -w $(GO_SOURCES)

fmt-check: ## List Go files that need formatting
	@gofmt -l $(GO_SOURCES)

vet: ## Go vet
	@go vet ./...

tidy: ## Tidy go.mod
	@go mod tidy

# ── Browser verification (needs a running gateway + console) ─────────────────

# Colour mode for the audit: dark (default), light, or both. Passed inline —
# never exported, and never named LLMR_MODE, which is the gateway's run mode.
AUDIT_MODE ?= dark

audit: $(NODE_DEPS) ## Console audit: every route x 3 viewports (AUDIT_MODE=both)
	@AUDIT_MODE=$(AUDIT_MODE) node $(WEB)/scripts/audit.mjs http://localhost:$(WEB_PORT)

verify: $(NODE_DEPS) ## Browser suite: audit + interactions + e2e + CRUD + observability + guards
	@AUDIT_MODE=both node $(WEB)/scripts/audit.mjs http://localhost:$(WEB_PORT)
	@node $(WEB)/scripts/interact.mjs http://localhost:$(WEB_PORT)
	@node $(WEB)/scripts/e2e.mjs      http://localhost:$(WEB_PORT) http://localhost:$(PORT)
	@node $(WEB)/scripts/crud.mjs     http://localhost:$(WEB_PORT) http://localhost:$(PORT)
	@node $(WEB)/scripts/obs.mjs      http://localhost:$(WEB_PORT) http://localhost:$(PORT)
	@node $(WEB)/scripts/guards.mjs   http://localhost:$(WEB_PORT) http://localhost:$(PORT)
	@node $(WEB)/scripts/analytics.mjs http://localhost:$(WEB_PORT) http://localhost:$(PORT)

# Re-records docs/media/*.gif: a fresh compose stack (the setup screen only shows on an empty
# database), the fake Anthropic upstream on :9901 so no provider keys or spend are involved, then
# the Playwright recorder. Needs ffmpeg. Wipes the compose stack's database.
demo-gifs: $(NODE_DEPS) ## Re-record the README demo GIFs (docs/media) against a fresh stack
	@-$(COMPOSE) -f deploy/docker-compose.yml down -v
	@set -e; LLMR_SEED_CLAUDE_SUBSCRIPTION=0 ./deploy/quickstart.sh; \
	python3 scripts/anthropic-stub.py 9901 >/dev/null 2>&1 & stub=$$!; trap 'kill $$stub' EXIT; \
	node $(WEB)/scripts/demo.mjs http://localhost:8080

# The playground suite needs a real password user: bootstrap-token sessions have
# no user row and so cannot persist conversations.
#   make verify-chat LLMR_USER=you@example.com LLMR_PASS=…
verify-chat: $(NODE_DEPS) ## Playground suites: persistence, attachments, paste, images
	@node $(WEB)/scripts/chat.mjs   http://localhost:$(WEB_PORT) http://localhost:$(PORT)
	@node $(WEB)/scripts/attach.mjs   http://localhost:$(WEB_PORT) http://localhost:$(PORT)
	@node $(WEB)/scripts/insights.mjs http://localhost:$(WEB_PORT) http://localhost:$(PORT)

# ── Housekeeping ─────────────────────────────────────────────────────────────

tools: $(AIR) ## Install dev tools (air)

$(AIR):
	@echo "  installing air…"
	@go install github.com/air-verse/air@latest

deps: $(NODE_DEPS) ## Install console dependencies

$(NODE_DEPS): $(WEB)/package.json
	@npm --prefix $(WEB) install
	@touch $@

clean: ## Remove build output
	@rm -rf tmp $(BINARY) $(WEB)/dist $(WEB)/*.tsbuildinfo

distclean: clean ## Also remove node_modules
	@rm -rf $(WEB)/node_modules

db-url: ## Print the database URL make dev will use
	@echo "$(LLMR_DATABASE_URL)"

# ── Containers (podman by default, docker if podman is absent) ───────────────

stack: ## Start the dev stack: Postgres, Zitadel (OIDC), Langfuse
	@$(COMPOSE) -f $(DEV_STACK) up -d
	@$(MAKE) --no-print-directory db-wait
	@printf "\n  Postgres  localhost:5433\n  Zitadel   http://localhost:8082\n  Langfuse  http://localhost:3001\n\n"

stack-down: ## Stop the dev stack (volumes are kept)
	@$(COMPOSE) -f $(DEV_STACK) down

stack-logs: ## Follow dev stack logs
	@$(COMPOSE) -f $(DEV_STACK) logs -f

# The gateway creates its own tables on startup but cannot create the database
# itself, so block until Postgres is actually accepting connections.
db-wait: ## Wait until Postgres is ready (starts the stack if it is not running)
	@if ! $(ENGINE) exec $$($(ENGINE) ps -q -f name=$(DB_SERVICE) | head -1) pg_isready -U $(DB_NAME) >/dev/null 2>&1; then \
	  echo "  Postgres not ready — starting the dev stack…"; \
	  $(COMPOSE) -f $(DEV_STACK) up -d $(DB_SERVICE) >/dev/null; \
	fi
	@for i in $$(seq 1 60); do \
	  $(ENGINE) exec $$($(ENGINE) ps -q -f name=$(DB_SERVICE) | head -1) pg_isready -U $(DB_NAME) >/dev/null 2>&1 && exit 0; \
	  sleep 1; \
	done; \
	echo "  ✗ Postgres did not become ready; try: make stack-logs"; exit 1

db-shell: ## psql into the gateway database
	@$(ENGINE) exec -it $$($(ENGINE) ps -q -f name=$(DB_SERVICE) | head -1) psql -U $(DB_NAME) -d $(DB_NAME)

# Destructive: drops every table so the next start replays all migrations from
# scratch. Useful for verifying migrations and for a clean slate.
db-reset: db-wait ## DESTRUCTIVE: drop and recreate the gateway schema
	@printf "  This deletes ALL data in the '$(DB_NAME)' database. Continue? [y/N] "; \
	  read ans; [ "$$ans" = "y" ] || { echo "  aborted"; exit 1; }
	@$(ENGINE) exec $$($(ENGINE) ps -q -f name=$(DB_SERVICE) | head -1) \
	  psql -U $(DB_NAME) -d $(DB_NAME) -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;' >/dev/null
	@echo "  schema dropped — migrations replay on the next gateway start"

image: ## Build the container image (IMAGE=llm-router:dev)
	@$(ENGINE) build -t $(IMAGE) -f Dockerfile .
	@$(ENGINE) images $(IMAGE) --format "  built {{.Repository}}:{{.Tag}}  {{.Size}}"

image-run: image ## Run the built image against the dev Postgres
	@$(ENGINE) run --rm -p $(PORT):8080 \
	  -e LLMR_DATABASE_URL="postgres://llmrouter:llmrouter@host.containers.internal:5433/llmrouter" \
	  -e LLMR_ADMIN_TOKEN="$(LLMR_ADMIN_TOKEN)" \
	  -e LLMR_ENCRYPTION_KEY="$(LLMR_ENCRYPTION_KEY)" \
	  $(IMAGE)
