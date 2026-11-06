# Deploying

One binary everywhere. What changes is where Postgres lives and who terminates TLS.

## Docker Compose

```bash
git clone https://github.com/exitcodenihil/llm-router && cd llm-router
./deploy/quickstart.sh
```

This writes `deploy/.env` once, starts Postgres and the gateway, and prints the console URL. **Back
up `deploy/.env`**: `LLMR_ENCRYPTION_KEY` protects stored provider keys, and a different key makes
them unreadable. Use `--build` to build from source, and `LLMR_VERSION=0.1.0` in `deploy/.env` to
pin a release. Only 8080 (console and API) and 8081 (workspace IDEs) are published, plus 3001 on
loopback if you turn on [Langfuse](#langfuse-in-compose). For [workspaces](workspaces.md), run
`./deploy/quickstart.sh --workspaces`.

For HTTPS, add Caddy. `deploy/Caddyfile`:

```
router.example.com {
	reverse_proxy llmrouter:8080
}
```

`deploy/docker-compose.caddy.yml`:

```yaml
services:
  caddy:
    image: caddy:2
    restart: unless-stopped
    ports: ["80:80", "443:443"]
    volumes: ["./Caddyfile:/etc/caddy/Caddyfile:ro", "caddy-data:/data"]
volumes:
  caddy-data:
```

```bash
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.caddy.yml up -d
```

With workspaces on, add `-f deploy/docker-compose.workspaces.yml`.

## Kubernetes

The Helm chart in `deploy/helm/llm-router` runs llm-router. Bring your own Postgres and point at an
existing empty database; llm-router creates its schema on start.

```bash
kubectl create secret generic llm-router \
  --from-literal=database-url='postgres://user:pass@host:5432/llmrouter' \
  --from-literal=admin-token="$(openssl rand -hex 16)" \
  --from-literal=encryption-key="$(openssl rand -hex 16)"

helm install llm-router ./deploy/helm/llm-router --set existingSecret=llm-router \
  --set ingress.enabled=true,ingress.host=router.example.com,ingress.className=nginx
```

Add `ingress.tls` for HTTPS. Set `trustedProxies` to your ingress controller's addresses, so sign-in
limits count each client rather than the ingress (`--set 'trustedProxies={10.0.0.0/8}'`).

Keep one control-plane replica and scale with [gateways](#gateways). Rate limits, budget counters,
live metrics and telemetry queues live in its memory, so the chart refuses `replicas` above 1 unless
you set `allowMultipleReplicas=true`, and then each replica enforces limits on its own traffic only. For
[workspaces](workspaces.md#kubernetes) add `--set workspaces.enabled=true,workspaces.ideOrigin=https://ide.example.com`.

## Gateways

Gateways serve your traffic where it is: behind a load balancer for throughput, in another region,
or dedicated to one team. They take everything else from the control plane: keys, models,
providers, prices and limits. A gateway is a small Rust binary,
`ghcr.io/exitcodenihil/llm-router-gateway`.

In the console open **Gateways**, create a group and copy its join token. Then start as many
gateways as you need with it:

```bash
docker run -p 8080:8080 \
  -e LLMR_CONTROL_PLANE_URL=https://router.example.com \
  -e LLMR_JOIN_TOKEN=llmrg_… \
  ghcr.io/exitcodenihil/llm-router-gateway
```

On Kubernetes, add `--set gateway.enabled=true,gateway.joinToken=llmrg_…` to the chart, which runs two
replicas behind their own service. Each gateway joins the group as its own instance and shows up on the
Gateways page. Clients use it
exactly like the control plane's own `/v1`. A group can:

- **serve only some teams.** Their keys, and their members' own keys, work; others get a 401. A
  team's gateways receive only the providers their models need.
- **prefer a region.** Deployments with the same region label are tried first, then the rest in
  the usual order.
- **limit telemetry.** Never capture prompts and responses, or never export at all.

Limits and budgets stay on keys, users and teams, and hold across every gateway. Each gateway syncs
its usage and counts with the control plane every 2 seconds, so a key can exceed a limit by at most
what all gateways admit in that time.

Config changes reach gateways within about a second. A gateway needs the control plane to start.
After that it keeps serving on its last config if the control plane goes away, and uploads the
usage it held (up to 200,000 events) when it's back.

A new join token stops further gateways joining with the old one; running ones carry on. Deleting a
group stops its gateways getting config at once, and they stop serving within 15 minutes.

Gateways trace requests as the control plane's own `/v1` does: they continue a caller's
`traceparent`, return `X-Llmr-Trace-Id`, and capture prompts and responses only when the capture
policy and a destination that takes the group's traffic with content both want them. Captured content
travels to the control plane with the usage it belongs to. A gateway keeps at most 64 MiB of it,
counting what requests in flight may still capture and the batch it is sending; past that it sends
usage without content.

Google Vertex AI and Azure Entra ID providers need gateways from 0.3.0; older gateways don't receive
them, and **Gateways** marks each one with the providers it skips. A Vertex provider's service account key goes to the gateways that serve it. With application
default credentials or Entra ID, each gateway uses its own identity, the same way the control plane
does:

- **Entra ID:** a client secret (`AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, `AZURE_CLIENT_SECRET`), then
  workload identity (`AZURE_FEDERATED_TOKEN_FILE`), then App Service or Container Apps managed
  identity, then the VM or AKS managed identity. `AZURE_CLIENT_ID` alone picks a user-assigned
  managed identity. `AZURE_AUTHORITY_HOST` selects a sovereign cloud.
- **Google:** `GOOGLE_APPLICATION_CREDENTIALS` (a service account key or an `authorized_user` file),
  then gcloud's application default credentials file, then the metadata server.

Certificate credentials for Entra ID and Google `external_account` files aren't supported on gateways.
A gateway logs why it refuses them, and requests to those providers fail with that reason.

| Variable | Meaning |
|---|---|
| `LLMR_CONTROL_PLANE_URL` | The control plane. Use https unless both are on a private network |
| `LLMR_JOIN_TOKEN` | The group's join token |
| `LLMR_LISTEN` | Listen address, default `0.0.0.0:8080` |
| `LLMR_WORKERS` | Worker threads; default: the container's CPU allowance |
| `LLMR_MAX_IN_FLIGHT` | Above this many requests in flight, answer 503 with `Retry-After` (default 10000) |
| `LLMR_SHUTDOWN_TIMEOUT` | Seconds to finish requests on SIGTERM before exiting (default 30) |
| `LLMR_GATEWAY_NAME` | Name on the Gateways page (default: the hostname) |

`/healthz` answers 200 once the gateway has its config and has synced, and 503 while it starts or
drains. `/metrics` is in Prometheus format; see [Monitoring](monitoring.md).

## Configuration

For llm-router itself (the gateways have [their own](#gateways)):

| Variable | Meaning |
|---|---|
| `LLMR_DATABASE_URL` | Postgres URL (required) |
| `LLMR_ENCRYPTION_KEY` | Encrypts provider keys and signs sessions; 16+ characters, required |
| `LLMR_ADMIN_TOKEN` | Bootstrap admin token, 12+ characters (optional) |
| `LLMR_LISTEN` | Listen address, default `:8080` |
| `LLMR_TRUSTED_PROXIES` | Comma-separated addresses or CIDR ranges of proxies whose `X-Forwarded-For` names the client. Console sign-in is limited to 20 attempts a minute per client address (IPv6 per /64) and 5 failures per 15 minutes per email and address; wrong admin tokens on the API to 5 per 15 minutes per address. Each proxy must append to `X-Forwarded-For` |
| `LLMR_SEED_CLAUDE_SUBSCRIPTION` | `1` adds the [subscription pass-through](clients.md#on-your-claudeai-subscription) provider and models |
| `LLMR_IDE_LISTEN`, `LLMR_IDE_ORIGIN`, `LLMR_COOKIE_DOMAIN` | [Workspace IDE](workspaces.md) listener, its public origin, and the session cookie domain when it is another hostname |
| `LLMR_METRICS_LISTEN` | Serve `/metrics` on this address (e.g. `:9090`) instead of the main listener; `off` turns it off. See [Monitoring](monitoring.md) |
| `LLMR_WORKSPACE_NETWORK` | Docker network shared with workspaces, so a containerised llm-router reaches each IDE by name (set by `docker-compose.workspaces.yml`). Unset, the IDE is published on the host's loopback |

Placeholder secrets such as `change-me` are refused. Generate them with `openssl rand -hex 16`.

## Operating

- **Monitoring**: both the control plane and the gateways serve Prometheus metrics, the chart
  can add PodMonitors, alert rules and a gateway autoscaler, and there is a Grafana dashboard.
  Gateways scale horizontally; the control plane doesn't. See [Monitoring](monitoring.md).
- **Traces**: **Observability** exports a trace of each request to OTLP backends and Langfuse. A
  capture policy chooses which traffic is exported and whether prompts leave (off by default), and
  each destination takes its teams, gateway groups and content mode. See
  [Observability](observability.md). Need a Langfuse to point it at?
  [Compose can run one](#langfuse-in-compose).
- **Response headers**: every `/v1` response from an upstream carries `X-Request-Id`,
  `X-Llmr-Provider`, `X-Llmr-Upstream` and `X-Llmr-Attempts`. A successful one also carries the
  answering upstream's `anthropic-ratelimit-*` and `x-ratelimit-*` headers. On a shared provider
  key those describe that key's quota, not the caller's.
- **Upgrades**: pull the new tag and restart. Migrations apply on start and only go forward, so
  back up Postgres first. Upgrade the control plane before the gateways.

### Upgrading to 0.3

- **Order**: the control plane first, then gateways. 0.2 gateways keep working with a 0.3 control
  plane (without Vertex AI, Entra ID, identity tokens or prompt capture); a 0.3 gateway can't join a
  0.2 control plane.
- **Telemetry**: Langfuse settings move to a destination on first start, and the telemetry API
  changes. Langfuse Cloud turns off the API 0.2 used on 16 November 2026. See
  [Observability](observability.md#upgrading-from-02).
- **Metrics**: the control plane now serves `/metrics`, unauthenticated, on 8080 unless
  `LLMR_METRICS_LISTEN` moves it (the chart and `deploy/docker-compose.yml` put it on 9090, which
  they don't expose). See [Monitoring](monitoring.md).
- **Identity token issuers** must use https (http only on localhost). An existing http issuer keeps
  working on the control plane but is refused by 0.3 gateways: change it to https.
- **Helm**: more than one control-plane replica is refused unless `allowMultipleReplicas=true`, and
  gateways now get a 50-second termination grace period and, on Kubernetes 1.30 or later, a 5-second
  pre-stop sleep (`gateway.terminationGracePeriodSeconds`, `gateway.preStopSleepSeconds`).
- **Sign-in limits** count per client address. Behind a proxy or ingress set `LLMR_TRUSTED_PROXIES`
  (chart: `trustedProxies`), or every client shares the proxy's limit.
- **Prices**: Claude Sonnet 5.5 cache reads are corrected to $0.20 per million tokens; catalog
  prices you edited are kept.

### Langfuse in Compose

`deploy/docker-compose.yml` ends with a commented-out Langfuse: web, worker, Postgres, ClickHouse,
Redis and MinIO, about 2 GB of RAM. Uncomment the services and the four `langfuse-*` volumes, then:

```bash
cd deploy
for v in NEXTAUTH_SECRET SALT ENCRYPTION_KEY PASSWORD SECRET_KEY; do
  echo "LANGFUSE_$v=$(openssl rand -hex 32)"; done >> .env
docker compose up -d
```

In the console, **Observability**, **Add destination**, **Langfuse**: host `http://langfuse-web:3000`,
UI URL `http://localhost:3001`, public key `pk-lf-llm-router`, secret key `LANGFUSE_SECRET_KEY` from
`deploy/.env`. Langfuse itself is at http://localhost:3001 (loopback only), login
`admin@example.com` and `LANGFUSE_PASSWORD`. Sign-up is off.


## Production checklist

- Managed Postgres with backups.
- `LLMR_ENCRYPTION_KEY` in a secret manager and backed up. Rotating it means re-entering provider
  keys and telemetry destination secrets, and signing everyone out.
- TLS in front of 8080.
- `/metrics` off the public listener (`LLMR_METRICS_LISTEN`; the chart does it), and the
  [alerts](monitoring.md#alerts) wired to someone. Give gateways a memory limit.
- Create real admin accounts or SSO, then unset `LLMR_ADMIN_TOKEN`.
- Leave [workspaces](workspaces.md) off unless you need them. They need a container socket or
  cluster credentials, which is host-level trust.
- Set `LLMR_IDE_ORIGIN` if you use workspaces, and don't expose 8081 otherwise.
