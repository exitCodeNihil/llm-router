# Monitoring

llm-router has two kinds of process to watch: the control plane (the `llm-router` image, one
replica) and the gateways (`llm-router-gateway`, as many as you run). Both serve Prometheus metrics
at `/metrics`, and the console's **Gateways** page shows each gateway's last hour without any of it.

The number that matters most is a gateway's memory against its container's limit. Past saturation
a gateway's memory grows with its open client connections, about 150 to 350 KB each (see
[benchmarks](benchmarks.md#rust-gateway-memory)), until the container is killed. Watch open
connections and memory together, and add gateways before CPU runs out.

## Endpoints

| Process | Where | Credential |
|---|---|---|
| Gateway | `GET /metrics` on its listener (`LLMR_LISTEN`, 8080) | none |
| Control plane | `GET /metrics` on its listener (8080), or on `LLMR_METRICS_LISTEN` if set | none |
| Console | **Gateways** page; `GET /api/nodes` and `GET /api/nodes/history` | admin |

`/metrics` carries no secret and nothing per key, user, team or model: counters by route and
status class, gauges, and the names of gateway groups, gateways and telemetry destinations. Like the gateways', the
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
| `llmr_sync_refused_events_total` | counter | | Usage events dropped because the control plane refused their batch, or a proxy refused one even alone and without content as too large |
| `llmr_usage_queued_content_bytes` | gauge | | Captured prompts and responses the queued usage events hold |
| `llmr_usage_sending_content_bytes` | gauge | | Captured prompts and responses in the usage batch being sent to the control plane, until it is acknowledged |
| `llmr_usage_inflight_content_bytes` | gauge | | Room for captured content that requests in flight have reserved: their prompt as captured, and their response up to the capture cap. With the queued content and the batch being sent, at most 64 MiB |
| `llmr_usage_content_stripped_total` | counter | | Usage events sent without their captured content: that 64 MiB was full (not captured at all, or put back in the queue after a failed sync), or a proxy refused even the smallest batch (64 KiB) as too large |
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

The `llmr_gateway_*` series come from each gateway's sync, every 2 seconds (back to back while it
has usage to upload), so scraping the
control plane alone gives the whole fleet's connections and memory. `gateway` is the gateway's name
(`LLMR_GATEWAY_NAME`, by default its hostname, the pod name on Kubernetes); the label is not
`instance`, which Prometheus sets to the scraped target. A gateway that shuts down goes from these
series within 15 seconds; one that stops syncing without shutting down stays for 30 minutes, so
`LLMRouterGatewayNotSyncing` can fire and keeps firing, and then goes. Give every gateway in a group
its own name: two with the same name show as one. Scrape the gateways too for their request
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
| `gateway.autoscaling.targetMemoryUtilizationPercentage` | unset | A memory target too, against `gateway.resources.requests.memory` |
| `gateway.preStopSleepSeconds` | `5` | On scale-in or rollout, keep accepting while the pod leaves the Service (Kubernetes 1.30+; `0` turns it off) |
| `gateway.terminationGracePeriodSeconds` | `50` | Time to finish requests in flight (`LLMR_SHUTDOWN_TIMEOUT`, 30 s) and upload the last usage |

The scrape annotations are left out while `metrics.podMonitor.enabled` is on, so no target is
scraped twice.

## Alerts

| Alert | Fires when | For |
|---|---|---|
| `LLMRouterControlPlaneDown` | No control plane scraped (`absent(llmr_build_info{runtime="control_plane"})`) | 5m |
| `LLMRouterGatewayNotSyncing` | A gateway's last sync is more than 30 s old | 1m |
| `LLMRouterGatewaySyncFailing` | A gateway's syncs keep failing | 10m |
| `LLMRouterGatewayNotReady` | A gateway has had no config or no sync since it started (`llmr_ready == 0`) | 5m |
| `LLMRouterGatewayMemoryHigh` | A gateway uses more than 85% of its memory limit, from its own `/metrics`, so it fires while the control plane is down. Needs a memory limit on the gateway | 5m |
| `LLMRouterRequestsShed` | A gateway sheds requests (`rate(llmr_shed_total[5m]) > 0`) | 2m |
| `LLMRouterHighErrorRate` | More than 5% of `/v1` requests end in 5xx | 10m |
| `LLMRouterUsageDropped` | Usage events were dropped in the last 10 minutes | |
| `LLMRouterTelemetryDestinationFailing` | A telemetry destination keeps failing (`llmr_telemetry_destination_failing{destination}`) | 10m |
| `LLMRouterTelemetrySpansDropped` | A telemetry destination dropped spans in the last 10 minutes, by reason (info) | |

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
takes its config from the control plane and shares rate limits and budgets through it. A gateway
mostly waits on upstreams, so it can run out of memory holding connections while its CPU stays
low: scale on CPU and memory, and give gateways a memory limit, so the memory alert has something
to measure against. Memory and its limit are read from cgroup v2; on a cgroup v1 host memory is the
process's resident size and there is no limit.

The control plane doesn't scale out. It keeps rate limits, budget counters, live metrics and the
gateway history in memory, so it runs as one replica (see [Kubernetes](deploy.md#kubernetes)).
