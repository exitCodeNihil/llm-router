# llm-router

> **Status: early (v0.1).** It runs real traffic and is tested, but expect rough edges and
> breaking changes before 1.0.

A self-hosted LLM gateway. One endpoint speaks both the OpenAI and Anthropic protocols and routes
to Azure OpenAI / AI Foundry, Google Vertex AI, OpenRouter, vLLM, LM Studio or any
OpenAI-compatible server.

- Works with Claude Code, Cursor, OpenCode, pi and any SDK.
- Virtual API keys, teams, budgets, rate limits and SSO, with usage and cost for every request.
- Cheapest-first routing with failover across backends that share a model name.
- Runs Claude Code on your claude.ai subscription and records it without holding a credential.
- Optional per-user dev workspaces (VS Code and a coding agent) and edge nodes for multi-region.
- Go, one static binary, no database on the hot path: about 52,000 req/s on a laptop against a stub
  upstream ([`scripts/anthropic-stub.py`](scripts/anthropic-stub.py)); your numbers will vary.

## Quick start

You need Docker or podman.

```bash
git clone https://github.com/exitcodenihil/llm-router && cd llm-router
./deploy/quickstart.sh
```

It generates `deploy/.env` once (**back it up**), starts Postgres and the gateway, and prints the
URL. Open <http://localhost:8080>, create the first admin on the setup screen, then add a provider
and a model:

![Add a provider and a model](docs/media/quickstart.gif)

Mint a key under **API Keys → New key**. Its **Connect** button shows ready-to-paste config for
Claude Code, OpenCode, pi and curl:

![Mint a key and connect Claude Code](docs/media/keys-connect.gif)

Every request then shows up with tokens, cost, latency and which provider answered:

![Requests on the dashboard](docs/media/dashboard.gif)

### Docker without cloning

The image is published for amd64 and arm64 at `ghcr.io/exitcodenihil/llm-router` (`latest`, or pin
a release such as `:0.1.0`). It needs Postgres, so use Compose. In an empty directory:

```bash
# Keep this file: the encryption key protects stored provider credentials.
printf 'LLMR_ADMIN_TOKEN=%s\nLLMR_ENCRYPTION_KEY=%s\n' "$(openssl rand -hex 16)" "$(openssl rand -hex 16)" > .env
```

<details>
<summary><code>docker-compose.yml</code></summary>

```yaml
# docker-compose.yml
services:
  postgres:
    image: postgres:17
    environment:
      POSTGRES_USER: llmrouter
      POSTGRES_PASSWORD: llmrouter
      POSTGRES_DB: llmrouter
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U llmrouter"]
      interval: 2s
      timeout: 2s
      retries: 15

  llmrouter:
    image: ghcr.io/exitcodenihil/llm-router:latest
    ports:
      - "8080:8080"
      - "8081:8081" # workspace IDEs, on their own origin
    environment:
      LLMR_DATABASE_URL: postgres://llmrouter:llmrouter@postgres:5432/llmrouter
      LLMR_ADMIN_TOKEN: ${LLMR_ADMIN_TOKEN:?run the printf line above}
      LLMR_ENCRYPTION_KEY: ${LLMR_ENCRYPTION_KEY:?run the printf line above}
      LLMR_IDE_LISTEN: ":8081"
      LLMR_IDE_ORIGIN: http://localhost:8081
      LLMR_SEED_CLAUDE_SUBSCRIPTION: "1"
    depends_on:
      postgres:
        condition: service_healthy

volumes:
  pgdata:
```
</details>

```bash
docker compose up -d      # then open http://localhost:8080
```

<details>
<summary>Plain <code>docker run</code> instead</summary>

```bash
export LLMR_ADMIN_TOKEN=$(openssl rand -hex 16)
export LLMR_ENCRYPTION_KEY=$(openssl rand -hex 16)   # keep both

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
  ghcr.io/exitcodenihil/llm-router:latest
```
</details>

## Docs

| | |
|---|---|
| [Providers and models](docs/providers.md) | Add Azure, Vertex, OpenRouter, vLLM and more |
| [Connect your tools](docs/clients.md) | Claude Code (subscription or other providers), Cursor, SDKs |
| [Keys, teams and access](docs/access.md) | Budgets, model policy, SSO, cloud identity tokens |
| [Deploying](docs/deploy.md) | Compose, TLS, Kubernetes, edge nodes, configuration |
| [Workspaces](docs/workspaces.md) | Per-user VS Code containers |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately per
[SECURITY.md](SECURITY.md).

## License

Apache-2.0
