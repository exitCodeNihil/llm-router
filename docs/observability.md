# Observability

llm-router exports a trace of every request to the backends you choose: any OpenTelemetry
collector or backend that takes OTLP over HTTP (Grafana Tempo, Jaeger, Honeycomb, Datadog, an
OpenTelemetry Collector), and Langfuse. You set this up on the console's **Observability** page. It
covers traffic the control plane serves itself and traffic served by gateways.

For metrics and alerts on llm-router itself, see [Monitoring](monitoring.md).

## How it decides

Two layers decide what leaves, and a request is exported to a destination only when both allow it.

1. **Capture policy** (one per installation) decides which requests are exported at all and
   whether their content may be:
   - **Everything** exports every request. **Capture prompt content** decides whether prompts and
     completions may go with it.
   - **By rule** exports only requests that match a rule. Rules match a team, user, key or tag, in
     order, and the first enabled match decides. Each rule says whether to capture content and what
     percentage to sample. Sampling is by request id, so the control plane and every gateway make
     the same decision for the same request.
   - **Off** exports nothing.
2. **Destinations** each take part of what the policy exports:
   - **Teams**: all, selected, or all except some.
   - **Gateway groups**: all, selected, or all except some. **Control plane** stands for the
     traffic the control plane serves on its own `/v1`.
   - **Content**: **Metadata only** sends model, tokens, cost, latency and status, never prompts or
     completions. **Full content** also sends the system prompt, the messages and the completion,
     when the policy captured them, cut to the destination's size cap (64 KiB by default). A span
     whose content was cut has `llmr.content.truncated` set.

A gateway group's data rule (on **Gateways**) is applied on top: **Never export** keeps its traffic
out of every destination, and **Never capture prompts or responses** sends metadata only, whatever
the destinations ask for. **Follow the capture policy** adds nothing.

Content is captured only when at least one destination in scope wants it, and is never stored in
Postgres. A change to the policy, a destination or a data rule also applies to spans still queued
for delivery.

## Destinations

On **Observability**, **Add destination**, pick a kind, fill in its connection, choose the content
and scope, and **Test**: it sends one `llm-router.test` span and shows the result with a link to
the trace when the destination has one. Secret keys and header values are write-only: the console
shows that they are set, never what they are. Changing a destination's host means entering them
again.

### OTLP (beta)

| Field | Meaning |
|---|---|
| Endpoint | The OTLP/HTTP base URL; spans go to `<endpoint>/v1/traces`, e.g. `http://otel-collector:4318` |
| Headers | Sent with every export, e.g. `Authorization` or `X-Scope-OrgID`. Put credentials here, not in the URL |
| Traces URL | Optional: the full URL, when it isn't under `/v1/traces` |
| Trace link | Optional: a URL with `{trace_id}` in it, for the **Trace** link on the Requests page, e.g. `https://grafana.example.com/explore?...{trace_id}...` |

Spans are protobuf, gzip-compressed (sent uncompressed instead if the backend refuses gzip). OTLP
is marked beta because the OpenTelemetry GenAI semantic conventions it follows are still in
development, so attribute names may change in a later release.

### Langfuse

| Field | Meaning |
|---|---|
| Host | Your Langfuse, e.g. `https://cloud.langfuse.com`, `https://us.cloud.langfuse.com` or your own |
| Public key, secret key | The project's API keys, from its settings in Langfuse |
| UI URL | Optional: the address your browser reaches Langfuse at, when the host is an internal name (`http://langfuse-web:3000` in Compose). Used for trace links |

llm-router sends Langfuse OpenTelemetry spans (`/api/public/otel/v1/traces`, ingestion version 4),
so it works with Langfuse v3 and v4, self-hosted or Cloud. Each request is a trace with one
generation, with its model, usage, cost and, with full content, its input and output. The
**Test** button creates a real trace named `llm-router.test` in the project.

## Trace ids

A request that carries a W3C `traceparent` header is exported as part of the caller's trace, as a
child of the caller's span, so the model call shows up inside your application's own trace. Without
one, each request starts a new trace. The trace's sampled flag doesn't change what is exported:
only the capture policy decides that.

Every authenticated `/v1` response carries `X-Llmr-Trace-Id`, and the Requests page links each
request to its trace in each destination that has a trace link.

## What a span carries

The span is named `chat <model>` (or the operation for completions and embeddings), with the real
start and end time and these attributes:

- **OpenTelemetry GenAI:** `gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`,
  `gen_ai.response.model`, `gen_ai.request.stream`, `gen_ai.usage.input_tokens` (cache included),
  `gen_ai.usage.output_tokens`, `gen_ai.usage.cache_read.input_tokens`,
  `gen_ai.usage.cache_write.input_tokens`, and with full content `gen_ai.system_instructions`,
  `gen_ai.input.messages` and `gen_ai.output.messages`.
- **Outcome:** `http.response.status_code`, `error.type`, `server.address`, `user.id`.
- **llm-router:** `llmr.request.id`, `llmr.key.id`, `llmr.team.id`, `llmr.gateway.group.id`,
  `llmr.gateway.instance.id`, `llmr.provider.id`, `llmr.deployment.id`, `llmr.cost.usd`,
  `llmr.cost.unpriced` (no price is known for the model), `llmr.cost.notional_usd` (what
  subscription pass-through traffic would have cost at list prices), `llmr.cost.basis`,
  `llmr.usage.cache_write_1h.input_tokens`, `llmr.attempts`, `llmr.ttft_ms`, `llmr.inference_geo`,
  `llmr.speed`, `llmr.rule.id`, `llmr.content.truncated`.

`llmr.cost.basis` is the version of the pricing rules the cost was computed with: `1` is 0.3's,
which prices cache writes, long prompts, US-only inference and fast mode the way Anthropic bills
them (since 0.2.1). Requests recorded before 0.3 carry none; those from before 0.2.1 are
under-reported for Claude models that used those features, and can't be repriced.

Langfuse gets the same span with Langfuse's own attributes for the generation, usage, cost, input
and output.

## Delivery

llm-router sends spans to whatever URL an admin enters, from the control plane's network, and shows
the start of an error response in the console. Only admins can add destinations, the same trust as
a provider's base URL.

Each destination has its own queue, so a slow or failing one never holds up the others or the
requests themselves. Spans are sent in batches of up to 512, at least every 2 seconds.

- A destination that is down or answers 429, 502, 503 or 504 is retried with backoff (honouring
  `Retry-After`) for up to 10 minutes, then the spans are dropped as expired.
- Other errors (a wrong key, say) aren't retried. A batch the destination refuses is split to find
  the spans it can't take, and only those are dropped.
- A queue holds up to 20,000 spans or 64 MiB; past that the oldest are dropped.

The **Status** column shows each destination as healthy, failing, no data yet or disabled, with
the last error on hover. `/metrics` has `llmr_telemetry_destination_failing`, `_queued`,
`_delivered_total` and `_dropped_total` by destination, and the Helm chart's alert rules include
`LLMRouterTelemetryDestinationFailing` (see [Monitoring](monitoring.md)).

## Try it locally

The development Compose file runs both kinds of backend:

```bash
cd deploy
docker compose -f docker-compose.dev.yml --profile otel --profile langfuse up -d
```

- **Grafana with Tempo** (profile `otel`): add an OTLP destination with endpoint
  `http://localhost:4318` and trace link `http://localhost:3000/explore?left={"datasource":"tempo","queries":[{"query":"{trace_id}"}]}`.
  Grafana is at http://localhost:3000.
- **Langfuse v4** (profile `langfuse`, about 2 GB of RAM): add a Langfuse destination with host
  `http://localhost:3001`, public key `pk-lf-dev-1234` and secret key `sk-lf-dev-1234`. Langfuse is
  at http://localhost:3001, login `admin@example.com` / `Password1!`.

## Upgrading from 0.2

See also [Upgrading to 0.3](deploy.md#upgrading-to-03) for the rest of the release.

- Langfuse settings from 0.2 move to a Langfuse destination named `Langfuse` on first start, with
  full content, so what was exported before still is. Its secret key is encrypted with
  `LLMR_ENCRYPTION_KEY`, which must be set.
- 0.2 exported to Langfuse's legacy ingestion API, which Langfuse v4 rejects and Langfuse Cloud
  turns off on 16 November 2026. 0.3 uses Langfuse's OpenTelemetry endpoint, so upgrade before then.
- The capture policy and rules carry over unchanged. Sampling is now by request id instead of at
  random, so the same share of traffic is exported but a given request is exported everywhere or
  nowhere.
- API: `GET/PUT /api/settings/telemetry` now carries only the policy (`mode`, `capture_content`,
  `rules`); destinations have their own `/api/telemetry/destinations`. `POST /api/observability/test`
  is gone; use `POST /api/telemetry/destinations/test`.
- Gateways from 0.2 keep working with a 0.3 control plane, but only 0.3 gateways capture content.
  Upgrade the control plane first: a 0.3 gateway can't join a 0.2 control plane. Gateways from 0.2
  don't say when they shut down, so with the Helm chart's alerts on, `LLMRouterGatewayNotSyncing`
  fires for each one replaced, for up to 30 minutes.

## Adding a destination kind

A kind is a Go type implementing `telemetry.Kind` (`internal/telemetry/kind.go`): its name, label,
whether it is beta, its secret fields, validation, and a constructor for its `Sink`, which exports a
batch of records and optionally gives a trace link. Register it in `cmd/llmrouter/main.go` next to
`otlp.Kind{}` and `langfuse.Kind{}`. A kind that speaks OTLP can reuse `otlp.Builder` and
`otlp.Client`, as Langfuse does; the console builds its form from the kind's fields, so a new kind
needs no schema change and little or no console work.
