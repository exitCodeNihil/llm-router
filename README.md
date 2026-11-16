# llm-router

> **Note:** llm-router was built for internal use, and some of its features are specific to that
> setup. If something does not work or is buggy, or you need a feature or a fix, please
> [open an issue](https://github.com/exitCodeNihil/llm-router/issues/new/choose).

> **Status: pre-1.0 (0.4).** It runs real traffic and is tested, but expect rough edges and
> breaking changes before 1.0.

A self-hosted LLM gateway. One endpoint speaks both the OpenAI and Anthropic protocols and routes
to Azure OpenAI / AI Foundry, Google Vertex AI, Anthropic, OpenRouter, vLLM, LM Studio or any
OpenAI-compatible server.

- Works with Claude Code, Cursor, OpenCode, pi and any SDK.
- Virtual API keys, teams, budgets, rate limits and SSO, with usage and cost for every request.
  Keys rotate with a grace period, sessions can be revoked, and an audit log records who changed
  what ([access](docs/access.md)).
- Cheapest-first routing with failover across backends that share a model name, and gateway
  groups that can be held to one region.
- Request traces to any OpenTelemetry backend and Langfuse, per team and gateway group, with or
  without prompts, and with secrets and personal data redacted before they leave
  ([observability](docs/observability.md)); Prometheus metrics and alerts for the router itself
  ([monitoring](docs/monitoring.md)).
- Runs Claude Code on your claude.ai subscription and records it without holding a credential.
- Optional per-user dev workspaces (VS Code and a coding agent), and Rust gateways you run behind a
  load balancer, in other regions or per team, all configured from one place. Gateways keep
  serving, and keep their usage, while the control plane or Postgres is away.
- Several control planes can share one database, and the encryption key rotates without downtime
  ([deploying](docs/deploy.md)).
- No database on the hot path. The control plane is one static Go binary: 15,000 req/s on one vCPU
  and 0.12 ms added per request, against 125 req/s and 4.2 ms for LiteLLM. A Rust gateway reaches
  30,000 req/s on one vCPU ([benchmarks](docs/benchmarks.md)).

## How it fits together

```mermaid
flowchart LR
    tools["Claude Code, Cursor,<br/>OpenCode, SDKs"]
    people["Admins and users<br/>(browser)"]

    subgraph cp["Control plane (Go)"]
        console["Console and<br/>management API"]
        cpapi["/v1 API"]
    end

    subgraph gws["Gateways (Rust, optional)"]
        gwapi["/v1 API"]
        disk[("State on disk:<br/>config, spend, usage")]
    end

    db[("Postgres:<br/>config, keys, usage, audit")]
    providers["Model providers:<br/>Azure, Vertex AI, Anthropic,<br/>OpenRouter, vLLM, ..."]
    traces["OpenTelemetry backends,<br/>Langfuse"]
    prom["Prometheus,<br/>Grafana"]

    people --> console
    tools -->|"OpenAI or Anthropic protocol"| cpapi
    tools -->|"OpenAI or Anthropic protocol"| gwapi
    cpapi --> providers
    gwapi --> providers
    cp <--> db
    gwapi <-->|"config, limits, usage"| cp
    gwapi --- disk
    cp -->|"request traces"| traces
    prom -.->|"scrape /metrics"| cp
    prom -.->|"scrape /metrics"| gws
```

| Component | What it does |
|---|---|
| **Control plane** (`llm-router`, Go) | The console and management API, plus a `/v1` endpoint of its own. It holds the config in memory and reads Postgres only to refresh it, so requests never wait on the database. Run one, or several on the same database. |
| **Gateways** (`llm-router-gateway`, Rust) | Optional. They serve `/v1` close to your users: behind a load balancer, in another region or per team. They take keys, models, prices and limits from the control plane and send usage back, and keep working from disk when it is away. |
| **Postgres** | Config, keys, usage, spend and the audit log. The only state to back up. |
| **Model providers** | Whatever you add under Providers. Both paths call them directly. |
| **Telemetry and monitoring** | Request traces go from the control plane to OpenTelemetry backends and Langfuse. Prometheus scrapes the control plane and each gateway. |

A small installation is just the control plane and Postgres; add gateways when you need more
throughput or another region.

## Quick start

You need Docker or podman.

```bash
git clone https://github.com/exitcodenihil/llm-router && cd llm-router
./deploy/quickstart.sh
```

It generates `deploy/.env` once (**back it up**), starts Postgres and the gateway, and prints the
URL. Open <http://localhost:8080>, create the first admin on the setup screen, then add a provider
and a model. Want [workspaces](docs/workspaces.md) too? Add `--workspaces`.

![Add a provider and a model](docs/media/quickstart.gif)

Mint a key under **API Keys → New key**. Its **Connect** button shows ready-to-paste config for
Claude Code, OpenCode, pi and curl:

![Mint a key and connect Claude Code](docs/media/keys-connect.gif)

Every request then shows up with tokens, cost, latency and which provider answered:

![Requests on the dashboard](docs/media/dashboard.gif)

### Docker without cloning

The image is published for amd64 and arm64 at `ghcr.io/exitcodenihil/llm-router` (`latest`, or pin
a release such as `:0.4.0`). It needs Postgres, so use Compose. In an empty directory:

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
| [Deploying](docs/deploy.md) | Compose, TLS, Kubernetes, gateways, configuration |
| [Observability](docs/observability.md) | Request traces to OpenTelemetry backends and Langfuse |
| [Monitoring](docs/monitoring.md) | Prometheus metrics, alerts and a Grafana dashboard for the router |
| [Workspaces](docs/workspaces.md) | Per-user VS Code containers |

## Contributing

Bug reports, feature requests and pull requests are welcome:
[open an issue](https://github.com/exitCodeNihil/llm-router/issues/new/choose) or see
[CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities privately per [SECURITY.md](SECURITY.md).

## License

Apache-2.0
