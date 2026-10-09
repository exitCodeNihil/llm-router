# llm-router gateway

The data plane of llm-router, in Rust. It joins a control plane with a gateway group's token, serves
`/v1` (OpenAI and Anthropic) from the config it gets back, and syncs usage and limit counters with
the control plane every couple of seconds. [docs/deploy.md](../docs/deploy.md#gateways) covers
running it.

## Protocol

The control plane's side is `internal/gwsync`. Every call after the join carries the session as
`Authorization: Bearer`.

| Call | What it does |
|---|---|
| `POST /gw/v1/join` | With the group's `llmrg_…` token: registers an instance and returns a 15-minute session, the group, and the sync interval |
| `POST /gw/v1/renew` | Returns a fresh session while the instance still exists |
| `GET /gw/v1/config?wait=25` | Long-poll with `If-None-Match: "v<version>"`. Returns 304, or the group's config, gzipped |
| `POST /gw/v1/sync` | Gzipped `{batch, cursor, events, rates, stats}`. Returns the combined counters `{cursor, now_ms, minute, spend, rates}` |

- **Config** holds only what the group serves: keys (as SHA-256 hashes), users, teams, providers
  (with credentials), deployments already in the group's routing order, and prices. It holds no
  spend and no telemetry credentials.
- **Sync batches are numbered.** A batch is resent unchanged until it is acknowledged, and the
  control plane counts each number once.
- **Usage events** go to Postgres on the control plane, which also decides what telemetry to
  export.
- **Limits.** Each limited scope keeps the control plane's last combined counts, plus what this
  gateway admitted since: pending until the next sync, in flight until it is acknowledged. A
  sliding one-minute window over them decides.

## Layout

| File | Contents |
|---|---|
| `main.rs` | Environment, runtime sized to the CPU allowance, listener, drain on SIGTERM |
| `cp.rs` | Join, renew, config long-poll, sync loop |
| `config.rs` | Wire format and the hot-path model: keys resolved to users and teams, deployments to URLs and auth headers |
| `server.rs` | Routes, auth, limits, failover with timeouts, the OpenAI and Anthropic surfaces |
| `body.rs` | Response bodies, and the event-stream wrapper that emits usage when a stream ends |
| `translate.rs` | OpenAI ↔ Anthropic translation, checked against the Go gateway's output |
| `limits.rs` | Synced rate limits and budgets |
| `health.rs` | Cooldown after upstream failures |
| `usage.rs` | Usage events and pricing |
| `upstream.rs` | Upstream URLs and credentials per provider |
| `metrics.rs` | Prometheus metrics |

## Develop

```bash
cargo test                      # unit tests, and translation against the Go fixtures
cargo clippy --all-targets -- -D warnings
make image-gateway              # from the repo root: the container image
```

The translation fixtures in `internal/gateway/testdata/translate` come from the Go gateway:
`go test ./internal/gateway -run Golden -update` regenerates them.
