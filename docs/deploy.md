# Deploying

Three ways to run llm-router, smallest first. All of them are the same binary; only
where Postgres lives and who terminates TLS changes. Environment variables are listed in
[configuration.md](configuration.md#environment--flags).

| You want | Use |
|---|---|
| Try it, or run it for a team on one machine | [Docker Compose](#docker-compose) |
| Run it on a cluster | [Kubernetes with Helm](#kubernetes-with-helm) |
| Serve callers in another region | [Edge nodes](edge.md) |
| No containers | `go build -o llmrouter ./cmd/llmrouter` (the console is committed in `web/dist`, so no Node needed), then run `./llmrouter --mode=all` with `LLMR_DATABASE_URL`, `LLMR_ENCRYPTION_KEY` and `LLMR_ADMIN_TOKEN` set |

## Docker Compose

```bash
git clone https://github.com/exitcodenihil/llm-router && cd llm-router
./deploy/quickstart.sh
```

The script writes `deploy/.env` once (admin token and encryption key from `openssl rand`),
starts Postgres and the gateway, waits for `/healthz`, and prints the console URL. It
never overwrites `deploy/.env`: **back it up**. `LLMR_ENCRYPTION_KEY` protects stored provider
credentials, and starting with a different key makes them unreadable.

- `./deploy/quickstart.sh --build` builds the image from source instead of pulling it.
- Pin a release with `LLMR_VERSION=0.1.0` in `deploy/.env` (image tags have no `v` prefix).
- Postgres is not published to the host; only 8080 (console and API) and 8081 (workspace
  IDEs) are. Bind them to localhost when something else terminates TLS:
  change `"8080:8080"` to `"127.0.0.1:8080:8080"` in `deploy/docker-compose.yml`.

### TLS with Caddy

Put a reverse proxy in front for HTTPS. Save as `deploy/Caddyfile`:

```
router.example.com {
	reverse_proxy llmrouter:8080
}
# Only if you use workspaces:
ide.example.com {
	reverse_proxy llmrouter:8081
}
```

and as `deploy/docker-compose.caddy.yml`:

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

then `docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.caddy.yml up -d`.
With workspaces, also set in `deploy/.env`: `LLMR_IDE_ORIGIN=https://ide.example.com` and
`LLMR_COOKIE_DOMAIN=.example.com`.

## Kubernetes with Helm

The chart in [`deploy/helm/llm-router`](../deploy/helm/llm-router) runs the control plane
(`mode=all`) or an edge node (`mode=gateway`). It does not ship a database: bring a managed
Postgres (RDS, Cloud SQL, Azure Database, CloudNativePG, ...) and point `LLMR_DATABASE_URL`
at an existing empty database. The gateway creates its own schema on start.

Keep secrets out of Helm values by creating them yourself:

```bash
kubectl create secret generic llm-router \
  --from-literal=database-url='postgres://user:pass@host:5432/llmrouter' \
  --from-literal=admin-token="$(openssl rand -hex 16)" \
  --from-literal=encryption-key="$(openssl rand -hex 16)"

helm install llm-router ./deploy/helm/llm-router \
  --set existingSecret=llm-router \
  --set ingress.enabled=true,ingress.host=router.example.com \
  --set ingress.className=nginx
```

For TLS add `ingress.tls` (a standard Ingress `tls:` list) or let your ingress controller
issue certificates through `ingress.annotations`. Probes use `/healthz`. Keep `replicas: 1`
for the control plane; scale out with [edge nodes](edge.md).

`helm upgrade llm-router ./deploy/helm/llm-router --set image.tag=<version>` upgrades.

## Production checklist

- **Postgres**: managed, with backups. Everything (keys, budgets, usage, config) lives there.
- **`LLMR_ENCRYPTION_KEY`**: stored in your secret manager and backed up. Rotating it means
  re-entering provider keys and signing everyone out.
- **TLS** in front of 8080. API keys and the admin token travel in headers.
- **Admin token**: it is a bootstrap credential. Create real admin accounts (or configure
  SSO) in the console and unset `LLMR_ADMIN_TOKEN` when you no longer need it.
- **Workspaces** need a container runtime socket or a Kubernetes service account, which is
  host- or cluster-level trust. Leave them off unless you need them, and read
  [Workspaces](configuration.md#workspaces) first. Do not expose 8081 otherwise.
- **Upgrades**: pull the new tag and restart. Schema migrations apply on start and are
  forward-only, so take a database backup first; there is no downgrade.
