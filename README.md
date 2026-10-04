# llm-router

> **Status: early (v0.1).** It runs real traffic and is tested, but expect rough edges and
> breaking changes before 1.0.

An open-source, self-hosted LLM gateway. One endpoint that speaks both the **OpenAI** and
**Anthropic** protocols — so Claude Code, Cursor, OpenCode, pi and any SDK all work — and
routes to **Azure OpenAI / AI Foundry**, **Google Vertex AI** (Gemini, Claude and the Model
Garden MaaS models), **OpenRouter**, **vLLM**, **LM Studio**, or any OpenAI-compatible
backend. Virtual API keys, usage and cost tracking, budgets, teams, SSO, cheapest-first
routing with failover, per-user dev **workspaces** with VS Code and a coding agent, and an
edge-gateway mode for multi-region deployments.

It can also sit in front of your **claude.ai subscription**: Claude Code keeps its own
login, the gateway forwards it and records every request — tokens, cache hits, latency —
without holding any credential.

**Performance** (M4 Pro laptop, 50 concurrent clients, stub upstream such as
[`scripts/anthropic-stub.py`](scripts/anthropic-stub.py); your numbers will vary): ~52,000 req/s
through the full path — auth, rate limits, budget check, routing, proxy, usage capture —
at p95 1.7 ms gateway overhead, with every usage event persisted.

## Get started

You need Docker or podman. One command, no manual secrets:

```bash
git clone https://github.com/exitcodenihil/llm-router && cd llm-router
./deploy/quickstart.sh
```

It generates `deploy/.env` once (**back it up**: the encryption key protects stored provider
credentials), starts Postgres and the gateway, waits until it is healthy and prints the URL.
Open **http://localhost:8080**, create the first admin account on the setup screen (or sign in
with the printed admin token), and you are in. Port 8081 serves workspace IDEs on their own
origin. Deploying for real (TLS, Kubernetes, edge nodes)? See [docs/deploy.md](docs/deploy.md).

![Add a provider and a model](docs/media/quickstart.gif)

Then, in the console: **Providers → Add provider**, **Models → Add model**, **API Keys → New
key** — the dialog that shows the key also shows ready-to-paste config for Claude Code,
Cursor, OpenCode and the SDKs.

![Mint a key and connect Claude Code](docs/media/keys-connect.gif)

<details>
<summary>Prefer plain <code>docker run</code>?</summary>

```bash
export LLMR_ADMIN_TOKEN=$(openssl rand -hex 16)
export LLMR_ENCRYPTION_KEY=$(openssl rand -hex 16)   # keep both

docker build -t llm-router .
docker network create llmr

docker run -d --name llmr-postgres --network llmr \
  -e POSTGRES_USER=llmrouter -e POSTGRES_PASSWORD=llmrouter -e POSTGRES_DB=llmrouter \
  -v llmr-pgdata:/var/lib/postgresql/data postgres:17

docker run -d --name llm-router --network llmr -p 8080:8080 -p 8081:8081 \
  -e LLMR_DATABASE_URL=postgres://llmrouter:llmrouter@llmr-postgres:5432/llmrouter \
  -e LLMR_ADMIN_TOKEN=$LLMR_ADMIN_TOKEN \
  -e LLMR_ENCRYPTION_KEY=$LLMR_ENCRYPTION_KEY \
  -e LLMR_IDE_LISTEN=:8081 -e LLMR_IDE_ORIGIN=http://localhost:8081 \
  -e LLMR_SEED_CLAUDE_SUBSCRIPTION=1 \
  llm-router
```
</details>

### Use it with Claude Code — on your own claude.ai subscription

The compose file (and `LLMR_SEED_CLAUDE_SUBSCRIPTION=1`) registers a pass-through
provider for `api.anthropic.com` with the Claude model names, storing no credential.

1. **API Keys → New key** in the console. The dialog that shows the key also shows
   ready-to-paste config — pick **Claude Code**, then **My claude.ai subscription**.
2. Paste it into a shell and run `claude`:

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_CUSTOM_HEADERS="X-Llmr-Key: llmr_…"     # your gateway key
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1       # /model lists the gateway's models
claude
```

Claude Code keeps using your subscription — do **not** set `ANTHROPIC_AUTH_TOKEN` in this
mode — and every request now shows up on the dashboard with tokens, prompt-cache hit
rate and latency. This is for your own subscription and your own Claude Code, not a way
to share one subscription across a team. Details and caveats:
[docs/integrations.md](docs/integrations.md).

![Requests on the dashboard](docs/media/dashboard.gif)

### Use it with your own provider keys

**Providers → Add provider**: Azure OpenAI / AI Foundry (Entra ID or key), Google Vertex
AI (ADC or a service-account key), OpenRouter (key), or any OpenAI-compatible server. Then
**Models → Add model**: pick the provider, choose from what it serves (discovery lists
them, prices pre-filled), give it the name clients will call. Several backends can share a
name — the gateway routes to the cheapest and fails over when one is down or throttled.

Mint a key and call it like OpenAI:

```bash
curl localhost:8080/v1/chat/completions -H "Authorization: Bearer llmr_…" -d '{
  "model": "gemini-flash", "stream": true,
  "messages": [{"role":"user","content":"hello"}]}'
```

…or like Anthropic (`/v1/messages`), which is what Claude Code, Cursor and OpenCode
send. The same key's **Connect** button on the API Keys page generates the config for
each of them, with every model the key may call.

### Workspaces

**Workspaces → New workspace** gives you a container with VS Code and a terminal — the
[pi](https://pi.dev) coding agent is preconfigured, just type `pi` — from a template (`base`, `python`, `node`, `go`,
`java`, `infra`). Upload your ssh key and `.gitconfig` once under *Your files* and every
workspace has them. Needs a container socket configured under **Develop → Workspace settings**;
see [docs/configuration.md](docs/configuration.md#workspaces).

## Why

- **One API for every provider** — clients speak OpenAI or Anthropic; the router handles
  Azure deployment URLs, Vertex's rawPredict, Entra ID and Google auth, and on-prem backends.
- **Native cloud identity** — callers already on Azure/GCP can authenticate with their
  `az login` / `gcloud` identity token instead of a gateway API key.
- **Real cost tracking** — list-price catalogs for Azure, Google and OpenRouter (synced
  live), custom per-model pricing for vLLM/on-prem, budgets and rate limits per key, user
  and team.
- **Fast by design** — Go, a single static binary, and a hot path that never touches the
  database (config lives in an atomically-swapped in-memory snapshot).
- **Edge gateways** — stateless nodes that sync config from the control plane, enforce
  limits locally, and ship usage events asynchronously.

## Console

The binary serves a web console at `/`: usage dashboards (spend, tokens, latency, errors),
API key management with one-time secrets and per-client connect snippets, teams with
per-team admins, providers and models (with discovery and routing order), the price
catalog, workspaces, SSO (any OIDC IdP — Entra ID, Google, Okta), cloud
token issuers, edge node management, and a streaming playground with attachments, tools
and structured output. Every playground turn reports which provider served it, time to
first token, total latency and cost, and links to its row in the request log — and can
be sent as a virtual API key to verify that key's allowed models, budget and rate limits.
Create the first admin on the setup screen (or log in with `LLMR_ADMIN_TOKEN`), then
configure SSO in Settings.

Built with React 19 and [Astryx](https://astryx.atmeta.com); see [web/README.md](web/README.md).

## Native cloud identity

Callers on Azure/GCP can skip API keys entirely and authenticate with their platform
identity token — see [docs/cloud-tokens.md](docs/cloud-tokens.md):

```bash
TOKEN=$(az account get-access-token --resource api://llm-router --query accessToken -o tsv)
curl https://router.example.com/v1/chat/completions -H "Authorization: Bearer $TOKEN" ...
```

## Edge gateways

Run stateless gateway nodes near your callers; they sync config from the control plane,
enforce auth/budgets locally with zero hot-path round-trips, keep serving through control
plane outages, and ship usage back in batches — see [docs/edge.md](docs/edge.md).

## Docs

- [Deploying](docs/deploy.md) — Docker Compose, TLS, Kubernetes/Helm, production checklist
- [Configuration](docs/configuration.md) — env vars, providers, deployments, pricing, limits
- [Claude Code, Cursor, pi](docs/integrations.md) — client setup
- [Cloud identity tokens](docs/cloud-tokens.md) — Entra ID / GCP setup
- [Edge gateways](docs/edge.md) — multi-region deployment
- [Helm chart](deploy/helm/llm-router) — `mode=all` and `mode=gateway`

## Development

```bash
make stack    # Postgres + Zitadel (OIDC) + Langfuse, via podman or docker
make dev      # gateway with live reload (air) + console with HMR (Vite)
make build    # single binary with the console embedded
make check    # go vet, go test, console typecheck
make verify   # browser suites: every route, interactions, end-to-end, CRUD
make help     # all targets
```

`make dev` serves the console on <http://localhost:5173> (hot reload) and the gateway on
<http://localhost:8080>, and starts Postgres first if it is not already running.
Override any default inline, e.g. `make dev PORT=9000 LLMR_DATABASE_URL=postgres://…`.

Container tooling prefers **podman** and falls back to docker; force one with
`ENGINE=docker`. `make image` builds the distroless production image (~18 MB).

### Schema and migrations

The gateway **creates its own schema on startup** — there is no separate migrate step.
`internal/store` embeds `migrations/*.sql` into the binary and applies them in filename
order, forward-only, each in a transaction, recorded in a `schema_migrations` table.
Already-applied files are skipped, so restarts are safe. The price catalog is seeded the
same way.

It does **not** create the database itself — Postgres cannot create a database over a
connection to it. `POSTGRES_DB` in the compose file (or `createdb`) handles that; point
`LLMR_DATABASE_URL` at an existing empty database and the gateway does the rest.

`make db-reset` drops the schema so the next start replays every migration from scratch.

## Architecture

Single binary, two modes:

- `--mode=all` (default) — control plane (management API + console) and gateway together,
  backed by Postgres.
- `--mode=gateway` — stateless edge node: pulls a signed config snapshot from the control
  plane, validates keys/tokens locally, ships usage events back in batches.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately per
[SECURITY.md](SECURITY.md).

## License

Apache-2.0
