# Deploying

One binary everywhere. What changes is where Postgres lives and who terminates TLS.

## Docker Compose

```bash
git clone https://github.com/exitcodenihil/llm-router && cd llm-router
./deploy/quickstart.sh
```

This writes `deploy/.env` once, starts Postgres and the gateway, and prints the console URL. **Back
up `deploy/.env`**: `LLMR_ENCRYPTION_KEY` protects stored provider keys, and a different key makes
them unreadable. Use `--build` to build from source, and `LLMR_VERSION=0.4.0` in `deploy/.env` to
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

One control plane is enough for most installs: [gateways](#gateways) carry the traffic and keep
serving while it restarts. To keep the console, the API and config changes up through a node
failure, run more with `--set replicas=2` (see [Several control planes](#several-control-planes)). For
[workspaces](workspaces.md#kubernetes) add `--set workspaces.enabled=true,workspaces.ideOrigin=https://ide.example.com`.

The pods run as a non-root user with a read-only root filesystem, no added capabilities and the
runtime's default seccomp profile (`podSecurityContext`, `securityContext`; set either to `{}` where
the cluster assigns its own, as OpenShift does). Gateways get a PodDisruptionBudget when there is
more than one, so a node drain takes them one at a time.

`networkPolicy.enabled=true` adds NetworkPolicies, which need a network plugin that enforces them.
Incoming traffic: `networkPolicy.from` lists who may reach llm-router and the gateways on 8080 besides
pods in the release's namespace (your ingress controller, apps in other namespaces), and
`networkPolicy.metricsFrom` who may scrape. An empty list allows every source. Outgoing traffic is
left open unless `networkPolicy.egress.enabled=true`; DNS and the release's namespace are then
allowed, and `networkPolicy.egress.rules` must cover the rest:

- Postgres, and each provider's port (usually 443)
- telemetry destinations (OTLP is often 4317 or 4318)
- for workspaces or `anthropicIdentity.source=kubernetes`, the Kubernetes API server at its real
  address and port (often 6443: policies apply after the Service address is translated)
- for cloud identity, the metadata address `169.254.169.254` on port 80

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
  the usual order. With **Only this region** on, the group's gateways use no other deployments
  and are not sent their credentials; a model with no deployment in the region is not served
  there. The control plane's own `/v1` has no region and is not affected.
- **limit telemetry.** Never capture prompts and responses, or never export at all.

Limits and budgets stay on keys, users and teams, and hold across every gateway. Each gateway syncs
its usage and counts with the control plane every 2 seconds, so a key can exceed a limit by at most
what all gateways admit in that time.

Config changes reach gateways within about a second. A gateway needs the control plane to start,
unless it [keeps state on disk](#gateway-state-on-disk). After that, if the control plane goes away,
it keeps serving on its last config for as long as the outage lasts, holds budgets against the last
spend it was told plus its own, and uploads the usage it held (up to 200,000 events) when the
control plane is back. To bound that, set `LLMR_MAX_CONFIG_AGE`: a gateway whose config the control
plane has not confirmed for that many seconds answers `/v1` with 503 `config_stale` and fails
`/healthz` until the next config poll is answered. It polls about every 25 seconds.

A gateway that has to join again while a batch of usage is unacknowledged, with or without state on
disk, sends that batch's events to the control plane's replay, which stores each request once; the
batch's rate-limit counts are not sent. Renewing a session is not joining again: an unacknowledged
batch is then resent as the sync it was. A control plane before 0.4 has no replay. A gateway
concludes that only of a control plane that has never answered a replay and answers 404 three times
running; it then resends the batch as a sync, which counts it twice if the first attempt had been
stored. Once a control plane has answered a replay, a 404 is retried until the replay goes through.

Deleting a group ends its gateways' sessions: each notices at its next sync or config poll, within
seconds, and tries to join again. The control plane refuses that join, and a gateway that has been
refused for 15 minutes stops serving: `/v1` answers 503 `gateway_refused`, `/healthz` fails, and it
deletes the config and spend it saved on disk. With state on disk the 15 minutes run across
restarts, and a gateway that restarts while it is refused serves nothing until it can join. Until then it serves on its last config without syncing limits or usage.
It keeps trying to join every 30 seconds and serves again if a join succeeds. Only the control
plane's own refusal counts: a 401 from a proxy in between, an older control plane, or one that
cannot reach its database never stops a gateway.

A new join token stops gateways joining with the old one. Running gateways carry on with the session
they have, which they renew without the token. They need the token again when they restart, or when
the session lapses because the control plane was unreachable for longer than it had left (5 to 15
minutes); they are then refused as above and stop serving 15 minutes later. Give running gateways
the new token before that.

A gateway that stops syncing turns red on the Gateways page and disappears from it after 10 minutes.
Its row is deleted from the database after 7 days, when it joins, and in the twice-daily maintenance
pass. An admin can remove a stopped one at once with **Remove**; if it is in fact still running, it
is refused on its next sync and rejoins by itself as a new instance.

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
| `LLMR_MAX_IN_FLIGHT` | Above this many requests in flight, answer `/v1` with 503 and `Retry-After` (default 10000). Probes and `/metrics` are always answered |
| `LLMR_SHUTDOWN_TIMEOUT` | Seconds to finish requests on SIGTERM before exiting (default 30). Idle keep-alive connections close at once and the rest after their current request, so clients reconnect to another gateway |
| `LLMR_GATEWAY_NAME` | Name on the Gateways page (default: the hostname) |
| `LLMR_MAX_CONFIG_AGE` | Seconds a gateway serves after the control plane last confirmed its config; unset or 0: no limit. At least 120 |
| `LLMR_STATE_DIR` | Directory for [state on disk](#gateway-state-on-disk); unset: nothing is written |
| `LLMR_USAGE_CLIENT_IP` | `true` reports each request's client address with its usage; the control plane stores it only when its own `LLMR_USAGE_CLIENT_IP` is on. Default `false` |
| `LLMR_TRUSTED_PROXIES` | With the above: addresses or CIDR ranges of the proxies in front of the gateway whose `X-Forwarded-For` names the client. Empty: the connecting address is the client |
| `LLMR_SPOOL_MAX_BYTES` | With `LLMR_STATE_DIR`: the most bytes of unsent usage kept on disk (default 268435456, 256 MiB); 0 keeps none |
| `LLMR_LOG_LEVEL` | `debug`, `info` (default), `warn` or `error` |
| `LLMR_LOG_FORMAT` | `text` (default) or `json`, one object per line with `ts`, `level` and `msg`. Logs go to stderr |

`/healthz` answers 200 once the gateway has its config and has synced, and 503 while it starts or
drains, while its config is older than `LLMR_MAX_CONFIG_AGE`, and once the control plane has refused
it. `/livez` answers 200 for as long as the process runs: use it for liveness and `/healthz` for
readiness, as the chart does. `/metrics` is in Prometheus format; see [Monitoring](monitoring.md).

### Gateway state on disk

With `LLMR_STATE_DIR` (`gateway.state.enabled` in the chart) a gateway saves, at most every 30
seconds, the config it last received and, beside it, when the control plane last confirmed that
config and the spend of every budgeted key, user and team: the control plane's figure as of the last
sync plus what the gateway has counted since, written every 30 seconds and at shutdown, also while
the control plane is away. While the control
plane refuses the gateway, it also keeps since when. The files are encrypted with a key derived from
the join token.

A gateway that starts with these files serves at once, before it reaches the control plane, if the
config was confirmed within the last 24 hours (or within `LLMR_MAX_CONFIG_AGE`, when that is
shorter). Budgets continue from the saved spend; if none was saved, requests under a budget get 429
`budget_unknown` until the first sync, and the rest are served. It then joins as usual and takes the
control plane's config. Files it cannot read, for example after the join token changed, are deleted
and it starts as it would without them.

The directory also holds a spool of usage: before each sync, and every two seconds while a gateway
that started from these files has not joined yet, the events queued since the last time are
written to `spool/`, without captured prompts or responses, and deleted once the control plane has
acknowledged them. A gateway that starts and finds events there, left by a run that crashed or was
killed, sends them to the control plane before its first sync, and the control plane stores each
request once, however often it is sent. What a crash can still lose is the usage of requests that
finished since the last sync, about two seconds' worth, and of requests in flight when the process
died. Events replayed from the spool carry no prompts or responses. Replay needs a 0.4 control plane; against an
older one (three 404s running, and no replay ever answered) the files wait for a later start.
A gateway sends only events that are in the spool: one that finishes while the spool is being
written goes with the next sync. Past `LLMR_SPOOL_MAX_BYTES` new events are not spooled
(`llmr_spool_skipped_events_total`) until acknowledged ones make room, which only happens while the
control plane is unreachable for a long time: 256 MiB holds several hundred thousand events.

One gateway uses a directory at a time: a second one logs an error and runs without saved state
(`llmr_state_dir_locked` is 0). The chart uses an `emptyDir`, which survives a container restart but
not the pod, so a new or rescheduled pod still needs the control plane and a deleted pod's spool is
gone. Its `gateway.state.sizeLimit` is 512Mi, twice the spool's cap: Kubernetes evicts a pod
that goes over its `emptyDir` limit, so the config and files being written need room beside a full
spool. If you raise `LLMR_SPOOL_MAX_BYTES`, raise it too. What is on disk is described
in [SECURITY.md](../SECURITY.md#data-at-rest).

## Configuration

For llm-router itself (the gateways have [their own](#gateways)):

| Variable | Meaning |
|---|---|
| `LLMR_DATABASE_URL` | Postgres URL (required) |
| `LLMR_ENCRYPTION_KEY` | Encrypts provider keys and signs sessions; 16+ characters, required |
| `LLMR_ENCRYPTION_KEYS_OLD`, `LLMR_ENCRYPTION_KEY_NEXT` | Comma-separated keys, and one key, that are still read and verified but never written with: for [rotating the encryption key](#rotating-the-encryption-key) |
| `LLMR_ADMIN_TOKEN` | Bootstrap admin token, 12+ characters (optional) |
| `LLMR_LISTEN` | Listen address, default `:8080` |
| `LLMR_TRUSTED_PROXIES` | Comma-separated addresses or CIDR ranges of proxies whose `X-Forwarded-For` names the client. Console sign-in is limited to 20 attempts a minute per client address (IPv6 per /64) and 5 failures per 15 minutes per email and address; wrong admin tokens on the API to 5 per 15 minutes per address. Each proxy must append to `X-Forwarded-For` |
| `LLMR_DB_MAX_CONNS`, `LLMR_DB_CONSOLE_MAX_CONNS` | Postgres connections for usage writes, gateway sync and config (default 16), and for console reads (default 8). Their sum must fit under the database's `max_connections` |
| `LLMR_DB_CONSOLE_TIMEOUT` | Cancels a console query that runs longer, default `30s`; the console then answers 504 |
| `LLMR_LOG_LEVEL` | Least severe level logged: `debug`, `info` (default), `warn` or `error`. Health checks, metrics scrapes and gateway syncs are logged at `debug` |
| `LLMR_LOG_FORMAT` | `text` (default) or `json`, one object per line |
| `LLMR_USAGE_RETENTION_MONTHS` | Months of individual requests to keep, besides the current one; older months are deleted. `0` (default) keeps them for ever; otherwise at least 2. See [Usage data](#usage-data) |
| `LLMR_USAGE_CLIENT_IP` | `true` stores each request's client address with its usage row: the connecting address, or what a proxy in `LLMR_TRUSTED_PROXIES` forwarded for. For a gateway's requests it is the address the gateway reports. Default `false`: the address is personal data. Daily and hourly totals never hold it |
| `LLMR_ROLLUP_RETENTION_MONTHS` | Months of daily totals and per-day spend to keep. `0` (default) keeps them for ever; otherwise at least 3 |
| `LLMR_TELEMETRY_CONCURRENCY` | Batches sent at once to each [telemetry destination](observability.md#delivery), 1 to 16. Default `4` |
| `LLMR_TELEMETRY_DRAIN` | How long a shutdown keeps delivering queued telemetry, up to `5m`. Default `20s`; keep the pod's termination grace period above it plus 15 seconds |
| `LLMR_AUDIT_RETENTION_MONTHS` | Months of the [audit log](access.md#audit-log) to keep; older rows are deleted twice a day. `0` (default) keeps it for ever. Chart: `retention.auditMonths` |
| `LLMR_SEED_CLAUDE_SUBSCRIPTION` | `1` adds the [subscription pass-through](clients.md#on-your-claudeai-subscription) provider and models |
| `LLMR_IDE_LISTEN`, `LLMR_IDE_ORIGIN`, `LLMR_COOKIE_DOMAIN` | [Workspace IDE](workspaces.md) listener, its public origin, and the session cookie domain when it is another hostname |
| `LLMR_METRICS_LISTEN` | Serve `/metrics` on this address (e.g. `:9090`) instead of the main listener; `off` turns it off. See [Monitoring](monitoring.md) |
| `LLMR_WORKSPACE_NETWORK` | Docker network shared with workspaces, so a containerised llm-router reaches each IDE by name (set by `docker-compose.workspaces.yml`). Unset, the IDE is published on the host's loopback |

Placeholder secrets such as `change-me` are refused. Generate them with `openssl rand -hex 16`.

## Operating

- **Monitoring**: both the control plane and the gateways serve Prometheus metrics, the chart
  can add PodMonitors, alert rules and a gateway autoscaler, and there is a Grafana dashboard.
  Gateways scale horizontally; the control plane can run as several replicas. See
  [Monitoring](monitoring.md).
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

### Several control planes

Any number of control planes can run on one database. Each registers in Postgres every second.

- **Limits and budgets** hold across all of them: each hands the others the requests and tokens it
  counted, and they read spend from the same tables. A limit can be passed by what all of them
  admit within a second or two, the same margin gateways have.
- **Gateways** can reach any of them through one Service. A control plane that is a few seconds
  behind on a config change asks the gateway to try again instead of answering with old data.
  That needs 0.4 gateways: a 0.3 gateway does not say which config it runs on, so behind several
  control planes it can be handed an older config for a moment, and usage for a key the lagging
  one does not know yet is dropped. Upgrade the gateways before adding control planes.
- **The console** shows the same thing on each: live traffic, gateways and events are put together
  from all of them. Latency figures on the live pages are then averages of each one's median.
- **Per control plane**, not shared: sign-in attempt limits and the list of recently exported
  traces. Each also exports the telemetry of the requests and gateway syncs it handled.
- **Connections**: each opens its own pools, so the database needs room for
  `replicas × (LLMR_DB_MAX_CONNS + LLMR_DB_CONSOLE_MAX_CONNS)`. Use a direct connection or a
  session-mode pooler; a transaction-mode pooler breaks the locks that keep maintenance to one
  control plane at a time.
- **A restored database**: gateways follow it. After a restore to another server, a point-in-time
  recovery or a failover they take the restored config at once. After a restore in place (a
  volume snapshot), where Postgres looks the same and only the config version went back, they
  keep their newer config for about a minute, until a control plane has seen one stay ahead that
  long and starts the versions over.
- **Upgrades**: stop every control plane before starting a new release, unless that release's notes
  say it can run beside the one before. The chart does this by default (`rollingUpdate: false`).
  Set `rollingUpdate: true` for one-at-a-time restarts and configuration changes.
- `/metrics` has `llmr_control_planes` (how many are running) and
  `llmr_cluster_sync_errors_total` (rounds of sharing that failed; while they fail each control
  plane enforces limits on what it sees itself).

### Rotating the encryption key

`LLMR_ENCRYPTION_KEY` encrypts provider keys, telemetry destination secrets, the SSO client secret
and workspace files, and signs console sessions and gateway sessions. It can be replaced without
re-entering any of them or signing anyone out, in three rollouts. Do each on every control plane
before starting the next.

1. **Announce the new key.** Set it as `LLMR_ENCRYPTION_KEY_NEXT` (chart: `encryptionKeyNext`) and
   roll out. Nothing is re-encrypted and everything is still written under the current key, but
   each control plane can now read and verify what one with the new key writes.
2. **Make it current.** Set the new key as `LLMR_ENCRYPTION_KEY`, move the previous one to
   `LLMR_ENCRYPTION_KEYS_OLD` (chart: `encryptionKeysOld: [...]`), clear `NEXT`, and roll out.
   Control planes now encrypt and sign with the new key and still read and verify under the old.
   Once every running control plane has the new key, one of them re-encrypts the stored secrets
   under it, within a minute and again each minute while any are left. A secret edited meanwhile
   keeps the edit.
3. **Drop the old key.** When `llmr_secrets_old_key` reads 0 on every control plane, remove
   `LLMR_ENCRYPTION_KEYS_OLD` and roll out. Console sessions signed with the old key end then, so
   wait 12 hours after step 2 (the longest a session lasts) to sign nobody out. Gateway sessions
   are renewed under the new key within 15 minutes.

With `existingSecret`, put the keys in that Secret as `encryption-key-next` and
`encryption-keys-old` (comma-separated). The pods read the Secret only when they start: after
changing it, restart them (`kubectl rollout restart deployment/<release>-llm-router`). Do not
rotate during an upgrade from a release without key rotation: it cannot read what the new key
writes. Until step 3, a rollback to the previous step's configuration reads everything; a
secret that no configured key opens is left as it is and logged once.

### Upgrading to 0.4

- **Stop the old control plane before starting 0.4.** The first start builds usage totals from the
  requests already stored, and writes wait while it does; on a large database that takes a while.
  Gateways keep serving meanwhile and upload their usage afterwards. The chart now replaces the
  control-plane pod instead of rolling it, and gives its first start 10 minutes
  (`startupFailureThreshold`); with Compose, `up -d` already replaces it.
- **Postgres 15 or later** is required.
- **Back up first**, as for any upgrade: there is no way back to 0.3 except the backup.
- **Latency percentiles** on Analytics are now estimates read from histograms: right to the
  histogram's range (1 to 1.5 s, say) and interpolated inside it. Days on the charts are UTC days.
- **Connections**: llm-router now opens up to 24 connections by default (16 + 8), where 0.3 opened
  about one per CPU. Check the database's `max_connections`.
- **Connection poolers**: llm-router sets `statement_timeout` and
  `idle_in_transaction_session_timeout` as it connects, and takes session advisory locks for
  migrations and maintenance. Connect it to Postgres directly, or to a pooler in session mode. Behind
  PgBouncer add both parameters to `ignore_startup_parameters`; in transaction mode the locks do
  nothing, so run one control plane only.
- **Several control planes** can now run at once and share limits
  ([Several control planes](#several-control-planes)). The upgrade to 0.4 itself must not mix
  releases: stop every 0.3 control plane first.
- **Health checks**: `/readyz` is the readiness check (config loaded, not shutting down) and
  `/healthz` the liveness check. Neither depends on Postgres: with the database away llm-router
  keeps serving from memory and queues usage. The chart uses both.
- **SSO client secret**: it is now stored encrypted with `LLMR_ENCRYPTION_KEY`; the first start
  encrypts the one already there. Backups made before the upgrade still hold it in plaintext: if
  those backups are not trusted, rotate the client secret at the identity provider and enter the
  new one under **Settings → OIDC provider**.

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
- **Helm**: more than one control-plane replica was refused unless `allowMultipleReplicas=true`
  (0.4 removes that value and the limit), and gateways now get a 50-second termination grace period and, on Kubernetes 1.30 or later, a 5-second
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


## Usage data

Every request is one row in `usage_events`, about half a kilobyte with its indexes: roughly 40 GB a
day at 1,000 requests a second. The table is partitioned by month.

llm-router also keeps totals as it writes: `usage_hourly` and `usage_daily` hold one row per hour or
day for each combination of key, user, team, model, deployment, gateway and status. The Dashboard
and Analytics read these, so they stay fast and keep working after the individual requests are gone.
The Requests page, and looking up one request by id, read the individual requests.

By default nothing is deleted. Set a retention to bound the database:

- `LLMR_USAGE_RETENTION_MONTHS` (chart: `retention.usageMonths`) drops whole months of individual
  requests. With `3`, on any day in June the oldest requests kept are from 1 March.
- `LLMR_ROLLUP_RETENTION_MONTHS` (chart: `retention.rollupMonths`) does the same for daily totals
  and per-day spend. Budgets are unaffected: all-time spend is kept separately.
- Hourly totals are always kept three months; charts older than that have daily points.

Old months are dropped twice a day and at start, a partition at a time, without blocking writes.
`llmr_usage_partitions_dropped_total` counts them. Deleted data is gone: only a backup has it.

## Production checklist

- Managed Postgres (15 or later) with backups, and a [retention](#usage-data) that fits its disk.
- `LLMR_ENCRYPTION_KEY` in a secret manager and backed up. Replace it with the
  [three-step rotation](#rotating-the-encryption-key); changing it in one step makes every stored
  secret unreadable and signs everyone out.
- TLS in front of 8080.
- `/metrics` off the public listener (`LLMR_METRICS_LISTEN`; the chart does it), and the
  [alerts](monitoring.md#alerts) wired to someone. Give gateways a memory limit.
- Create real admin accounts or SSO, then unset `LLMR_ADMIN_TOKEN`.
- Leave [workspaces](workspaces.md) off unless you need them. They need a container socket or
  cluster credentials, which is host-level trust.
- Set `LLMR_IDE_ORIGIN` if you use workspaces, and don't expose 8081 otherwise.
