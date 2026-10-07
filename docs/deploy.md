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

The Helm chart in `deploy/helm/llm-router` runs the control plane (`mode=all`) or an edge node
(`mode=gateway`). Bring your own Postgres and point at an existing empty database; the gateway
creates its schema on start.

```bash
kubectl create secret generic llm-router \
  --from-literal=database-url='postgres://user:pass@host:5432/llmrouter' \
  --from-literal=admin-token="$(openssl rand -hex 16)" \
  --from-literal=encryption-key="$(openssl rand -hex 16)"

helm install llm-router ./deploy/helm/llm-router --set existingSecret=llm-router \
  --set ingress.enabled=true,ingress.host=router.example.com,ingress.className=nginx
```

Add `ingress.tls` for HTTPS. Keep one control-plane replica and scale with edge nodes. For
[workspaces](workspaces.md#kubernetes) add `--set workspaces.enabled=true,workspaces.ideOrigin=https://ide.example.com`.

## Edge nodes

Stateless gateways near your callers. They sync config from the control plane, check keys and
budgets locally, keep serving if the control plane goes down, and ship usage back in batches. In
the console open **Edge nodes**, register a node, and copy its one-time token, then:

```bash
LLMR_CONTROL_PLANE_URL=https://router.example.com LLMR_NODE_TOKEN=llmrn_… \
  llmrouter --mode=gateway --listen :8080
# or: helm install edge ./deploy/helm/llm-router \
#       --set mode=gateway,controlPlaneUrl=https://router.example.com,nodeToken=llmrn_…
```

Config changes reach nodes in about 2 seconds. Budgets and rate limits are enforced per node, so
with N nodes a budget can overshoot by about N times the spend per sync. Edge nodes can't create
users: a first-time user has to reach the control plane once.

## Configuration

| Variable | Used by | Meaning |
|---|---|---|
| `LLMR_DATABASE_URL` | control plane | Postgres URL (required) |
| `LLMR_ENCRYPTION_KEY` | control plane | Encrypts provider keys and signs sessions; 16+ characters, required |
| `LLMR_ADMIN_TOKEN` | control plane | Bootstrap admin token, 12+ characters (optional) |
| `LLMR_LISTEN` | all | Listen address, default `:8080` |
| `LLMR_MODE` | all | `all` (default) or `gateway` |
| `LLMR_CONTROL_PLANE_URL`, `LLMR_NODE_TOKEN` | edge | Where to sync from and how to authenticate |
| `LLMR_SNAPSHOT_CACHE` | edge | Config cache path (default `/var/lib/llmrouter/snapshot.json`) |
| `LLMR_SEED_CLAUDE_SUBSCRIPTION` | control plane | `1` adds the [subscription pass-through](clients.md#on-your-claudeai-subscription) provider and models |
| `LLMR_IDE_LISTEN`, `LLMR_IDE_ORIGIN`, `LLMR_COOKIE_DOMAIN` | control plane | [Workspace IDE](workspaces.md) listener, its public origin, and the session cookie domain when it is another hostname |
| `LLMR_WORKSPACE_NETWORK` | control plane | Docker network shared with workspaces, so a containerised gateway reaches each IDE by name (set by `docker-compose.workspaces.yml`). Unset, the IDE is published on the host's loopback |

Placeholder secrets such as `change-me` are refused. Generate them with `openssl rand -hex 16`.

## Operating

- **Langfuse**: **Observability** exports request traces. Rules choose which traffic is exported
  and whether prompts leave the gateway (off by default). With no rules, nothing is exported.
  Wrong credentials are rejected per batch, so check the delivery health on that page. Need a
  Langfuse to point it at? [Compose can run one](#langfuse-in-compose).
- **Response headers**: every `/v1` response from an upstream carries `X-Request-Id`,
  `X-Llmr-Provider`, `X-Llmr-Upstream` and `X-Llmr-Attempts`.
- **Upgrades**: pull the new tag and restart. Migrations apply on start and only go forward, so
  back up Postgres first.

### Langfuse in Compose

`deploy/docker-compose.yml` ends with a commented-out Langfuse: web, worker, Postgres, ClickHouse,
Redis and MinIO, about 2 GB of RAM. Uncomment the services and the four `langfuse-*` volumes, then:

```bash
cd deploy
for v in NEXTAUTH_SECRET SALT ENCRYPTION_KEY PASSWORD SECRET_KEY; do
  echo "LANGFUSE_$v=$(openssl rand -hex 32)"; done >> .env
docker compose up -d
```

In the console, **Observability**: host `http://langfuse-web:3000`, public key `pk-lf-llm-router`,
secret key `LANGFUSE_SECRET_KEY` from `deploy/.env`. Langfuse itself is at http://localhost:3001
(loopback only), login `admin@example.com` and `LANGFUSE_PASSWORD`. Sign-up is off.

- That host is the Compose-internal name, so the **Trace** links on the Requests page don't open
  from your browser. Open `http://localhost:3001/project/llm-router/traces/<request id>` instead.
- Keep the images on `:3`. Langfuse 4 rejects the ingestion API the gateway exports with.

## Production checklist

- Managed Postgres with backups.
- `LLMR_ENCRYPTION_KEY` in a secret manager and backed up. Rotating it means re-entering provider
  keys and signing everyone out.
- TLS in front of 8080.
- Create real admin accounts or SSO, then unset `LLMR_ADMIN_TOKEN`.
- Leave [workspaces](workspaces.md) off unless you need them. They need a container socket or
  cluster credentials, which is host-level trust.
- Set `LLMR_IDE_ORIGIN` if you use workspaces, and don't expose 8081 otherwise.
