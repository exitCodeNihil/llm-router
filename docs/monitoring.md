# Monitoring

llm-router has two kinds of process to watch: the control plane (the `llm-router` image, one
replica) and the gateways (`llm-router-gateway`, as many as you run). Both serve Prometheus metrics
at `/metrics`, and the console's **Gateways** page shows each gateway's last hour without any of it.

The number that matters most is a gateway's memory against its container's limit. Past saturation
a gateway's memory grows with its open client connections, about 150 to 300 KB each (see
[benchmarks](benchmarks.md#rust-gateway-memory)), until the container is killed. Watch open
connections and memory together, and add gateways before CPU runs out.

## Endpoints

| Process | Where | Credential |
|---|---|---|
| Gateway | `GET /metrics` on its listener (`LLMR_LISTEN`, 8080) | none |
| Control plane | `GET /metrics` on its listener (8080), or on `LLMR_METRICS_LISTEN` if set | none |
| Console | **Gateways** page; `GET /api/nodes` and `GET /api/nodes/history` | admin |

`/metrics` carries no secret and nothing per key, user, team or model: counters by route and
status class, gauges, and the names of gateway groups and gateways. Like the gateways', the
control plane's asks for no credential, so Prometheus scrapes it with no setup.

The control plane's listener also serves the console, so it is often public. To keep `/metrics` off
it, set `LLMR_METRICS_LISTEN` to another address, e.g. `:9090`, which your ingress does not route;
`/metrics` is then only served there. `LLMR_METRICS_LISTEN=off` turns it off. The Helm chart does
this by default (`metrics.port: 9090`). If you expose 8080 directly, either set it or block
`/metrics` at your proxy. A gateway's listener serves only `/v1`, `/healthz` and `/metrics`.

## Metrics

Both runtimes write these with the same names, so a sum over every scraped target is the fleet:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `llmr_requests_total` | counter | `route`, `code` | Requests served. `route` is `chat_completions`, `completions`, `embeddings`, `messages`, `models`, `count_tokens` or `other`; the control plane adds `api` (console and management API) and `gw_sync` (gateways' syncs). `code` is `1xx` to `5xx` |
| `llmr_request_duration_seconds` | histogram | | Latency of requests, streams to their end. On the control plane only `/v1` requests |
| `llmr_in_flight` | gauge | | Requests being served (on the control plane, `/v1` requests) |
| `llmr_open_connections` | gauge | | Client connections open |
| `llmr_connections_total` | counter | | Client connections accepted |
| `llmr_memory_bytes` | gauge | | Memory in use: the cgroup v2 working set (`memory.current` less inactive page cache, as `kubectl top` and `docker stats` count it), else the process's RSS |
| `llmr_memory_limit_bytes` | gauge | | The cgroup's `memory.max`. Absent when there is no limit |
| `llmr_usage_queued`, `llmr_usage_queue_capacity` | gauge | | Usage events waiting: for Postgres on the control plane, for the next sync on a gateway |
| `llmr_usage_dropped_total` | counter | | Usage events dropped because the queue was full |
| `llmr_build_info` | gauge | `runtime` (`control_plane`, `gateway`), `version` | Always 1 |

Gateways only:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `llmr_ready` | gauge | | 1 once it has its config and has synced |
| `llmr_shed_total` | counter | | Requests refused with 503 because more than `LLMR_MAX_IN_FLIGHT` were in flight |
| `llmr_upstream_attempts_total` | counter | `result` (`ok`, `error`, `timeout`, `retryable_status`) | Upstream attempts |
| `llmr_limit_denials_total` | counter | `reason` (`budget_exceeded`, `rate_limit_exceeded`) | Requests refused by a budget or rate limit |
| `llmr_sync_failures_total` | counter | | Failed syncs with the control plane |
| `llmr_sync_refused_events_total` | counter | | Usage events dropped because the control plane refused their batch |
| `llmr_config_version`, `llmr_workers` | gauge | | Config version in use; worker threads |

The control plane only:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `llmr_go_heap_bytes`, `llmr_go_mapped_bytes` | gauge | | Live heap; memory the Go runtime has mapped |
| `llmr_go_goroutines` | gauge | | Goroutines |
| `llmr_go_gc_cycles_total` | counter | | Completed GC cycles |
| `llmr_gateways_connected` | gauge | `group` | Gateways syncing with this control plane |
| `llmr_gateway_open_connections` | gauge | `group`, `gateway` | A gateway's open connections at its last sync |
| `llmr_gateway_in_flight` | gauge | `group`, `gateway` | A gateway's requests in flight at its last sync |
| `llmr_gateway_memory_bytes` | gauge | `group`, `gateway` | A gateway's memory in use at its last sync |
| `llmr_gateway_memory_limit_bytes` | gauge | `group`, `gateway` | A gateway's memory limit; absent without one |
| `llmr_gateway_last_sync_age_seconds` | gauge | `group`, `gateway` | Seconds since a gateway last synced |
| `llmr_gateway_info` | gauge | `group`, `gateway`, `version` | Always 1 |
| `llmr_telemetry_destination_failing` | gauge | `destination`, `kind` | 1 while a [telemetry destination](observability.md)'s last export failed |
| `llmr_telemetry_destination_queued` | gauge | `destination` | Spans waiting to be sent |
| `llmr_telemetry_destination_delivered_total` | counter | `destination` | Spans the destination accepted |
| `llmr_telemetry_destination_dropped_total` | counter | `destination`, `reason` | Spans lost: `overflow` (queue full), `rejected` (refused by the destination), `expired` (retries gave up) |

The `llmr_gateway_*` series come from each gateway's sync, about every second, so scraping the
control plane alone gives the whole fleet's connections and memory. `gateway` is the gateway's name
(`LLMR_GATEWAY_NAME`, by default its hostname, the pod name on Kubernetes); the label is not
`instance`, which Prometheus sets to the scraped target. A gateway that shuts down goes from these
series within 15 seconds; one that stops syncing without shutting down stays for 5 minutes, so
`LLMRouterGatewayNotSyncing` can fire, and then goes. Scrape the gateways too for their request
counters, latency, sheds and sync failures.

## Kubernetes

The chart annotates both pods with `prometheus.io/scrape`, `/port` and `/path`
(`metrics.podAnnotations`). With the Prometheus Operator, add PodMonitors and the alert rules:

```bash
helm upgrade llm-router ./deploy/helm/llm-router --reuse-values \
  --set metrics.podMonitor.enabled=true,metrics.prometheusRule.enabled=true \
  --set metrics.podMonitor.labels.release=kube-prometheus-stack \
  --set metrics.prometheusRule.labels.release=kube-prometheus-stack
```

The labels must match your Prometheus's `podMonitorSelector` and `ruleSelector`.

| Value | Default | Meaning |
|---|---|---|
| `metrics.port` | `9090` | The control plane's metrics port (`LLMR_METRICS_LISTEN`); `0` serves `/metrics` on 8080 |
| `metrics.podAnnotations` | `true` | `prometheus.io/*` scrape annotations on both pods |
| `metrics.podMonitor.enabled` | `false` | A PodMonitor for the control plane and one for the gateways |
| `metrics.podMonitor.interval`, `.labels` | `30s`, `{}` | Scrape interval; labels for your Prometheus to select them |
| `metrics.prometheusRule.enabled` | `false` | The alert rules below |
| `metrics.prometheusRule.labels` | `{}` | Labels for your Prometheus to select them |
| `metrics.prometheusRule.selector` | `namespace="<release namespace>"` | The series the alerts look at |
| `metrics.prometheusRule.gatewayMemoryRatio` | `0.85` | `LLMRouterGatewayMemoryHigh` threshold |
| `metrics.prometheusRule.errorRatio` | `0.05` | `LLMRouterHighErrorRate` threshold |
| `metrics.prometheusRule.notSyncingSeconds` | `30` | `LLMRouterGatewayNotSyncing` threshold |
| `gateway.autoscaling.enabled` | `false` | A HorizontalPodAutoscaler for the gateways |
| `gateway.autoscaling.minReplicas`, `.maxReplicas` | `2`, `10` | Its bounds |
| `gateway.autoscaling.targetCPUUtilizationPercentage` | `70` | Its CPU target, against `gateway.resources.requests.cpu` |

## Alerts

| Alert | Fires when | For |
|---|---|---|
| `LLMRouterControlPlaneDown` | No control plane scraped (`absent(llmr_build_info{runtime="control_plane"})`) | 5m |
| `LLMRouterGatewayNotSyncing` | A gateway's last sync is more than 30 s old | 1m |
| `LLMRouterGatewaySyncFailing` | A gateway's syncs keep failing | 10m |
| `LLMRouterGatewayMemoryHigh` | A gateway uses more than 85% of its memory limit | 5m |
| `LLMRouterRequestsShed` | A gateway sheds requests (`rate(llmr_shed_total[5m]) > 0`) | 2m |
| `LLMRouterHighErrorRate` | More than 5% of `/v1` requests end in 5xx | 10m |
| `LLMRouterUsageDropped` | Usage events were dropped in the last 10 minutes | |
| `LLMRouterTelemetryDestinationFailing` | A telemetry destination keeps failing (`llmr_telemetry_destination_failing{destination}`) | 10m |

Without the operator, copy the `groups` from `helm template` with
`metrics.prometheusRule.enabled=true` into a rules file.

## Grafana

`deploy/grafana/llm-router.json` is a dashboard on these metrics: request rate by route and status,
5xx share, latency p50/p95/p99, each gateway's open connections, requests in flight and memory
against its limit, sheds, seconds since each gateway's last sync, sync failures, upstream attempts,
and the control plane's usage queue, memory and connections.

Import it in Grafana under **Dashboards**, **New**, **Import**: upload the file, and pick your
Prometheus data source at the top of the dashboard. Or provision it from a ConfigMap with your
Grafana's dashboard sidecar. The **Namespace** variable narrows it to one release; series without a
`namespace` label (outside Kubernetes) show under **All**.

## The Gateways page

**Gateways** lists the control plane and every gateway seen in the last 10 minutes, with requests a
second, median latency, open connections, requests in flight, CPU against the CPU allowance, memory
against the limit, and uptime. Below, **Last hour** graphs each gateway's open connections, requests
in flight and memory: the peak of every 10 seconds, from the gateways' syncs. The history lives in
the control plane's memory, so it starts empty after a restart.

## Scaling

Gateways scale horizontally: add replicas, or turn on `gateway.autoscaling`. Each joins its group,
takes its config from the control plane and shares rate limits and budgets through it. Scale on CPU
and give gateways a memory limit, so the memory alert has something to measure against.

The control plane doesn't scale out. It keeps rate limits, budget counters, live metrics and the
gateway history in memory, so it runs as one replica (see [Kubernetes](deploy.md#kubernetes)).
