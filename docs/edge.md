# Edge gateways

For multi-region or on-prem-adjacent deployments, run lightweight edge gateways near your
callers. Edges hold the full routing/auth/pricing config in memory, so request handling
never leaves the node — the control plane is only needed asynchronously.

```
callers ──> edge gateway (region A) ──┐
callers ──> edge gateway (region B) ──┼── long-poll config / batch usage ──> control plane ──> Postgres
callers ──> control plane gateway  ───┘
```

## Setup

1. In the console (admin): Edge Nodes → create a node, copy the one-time token.
2. Run the same binary anywhere:

```bash
LLMR_CONTROL_PLANE_URL=https://router.example.com \
LLMR_NODE_TOKEN=llmrn_... \
llmrouter --mode=gateway --listen :8080
```

Or with Helm: `helm install edge ./deploy/helm/llm-router --set mode=gateway,controlPlaneUrl=...,nodeToken=...`

## Behavior

- **Config sync**: long-poll (`55s` hold) against the control plane; changes (new keys,
  revocations, deployments, prices) propagate within ~2 seconds.
- **Cold start / outages**: the last snapshot is cached on disk (0600, HMAC-sealed with the
  node token). An edge restarts and serves during a full control-plane outage; it reconciles
  when the control plane returns.
- **Usage**: events buffer in memory and ship in gzip batches every 5s; the control plane
  stamps them with the node id. Events buffered but not yet shipped are lost if the node
  crashes (in-memory spool).
- **Budgets**: each snapshot carries current spend per key/user/team; edges enforce budgets
  against that plus locally-observed spend. With N edge nodes the worst-case overshoot is
  roughly N × spend-per-sync-interval. Rate limits are enforced per node (approximate
  globally).
