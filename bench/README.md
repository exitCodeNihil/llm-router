# Gateway benchmark

llm-router against LiteLLM (Python proxy and its Rust gateway) and Otari, all in front of the same
mock upstream. Results: [docs/benchmarks.md](../docs/benchmarks.md).

## Run it

You need podman (with compose), Go, jq, curl and perl. The full comparison takes about 9 hours:

```bash
make bench             # builds this checkout, runs everything, rewrites the tables in docs/benchmarks.md
```

On a Mac, plug in and keep the lid open. The run refuses to start on battery, and if the Mac sleeps
anyway it marks the results `INVALID`, which `./run.sh docs` refuses to publish.

Step by step, from `bench/`:

```bash
./run.sh all           # results/<date>/; builds LiteLLM's Rust gateway from source the first time
./run.sh report        # every run as one table
./run.sh docs          # the summary tables into ../docs/benchmarks.md (prose stays as written)
```

- `GATEWAYS="router lrust"` runs only those.
- `ROUTER_IMAGE=localhost/llm-router:dev` benchmarks a local build instead of the pinned release.
- `RUN=2026-10-07` writes into, or adds to, an existing results folder.
- `./run.sh s3 router litellm` runs one scenario.
- `QUICK=1` shrinks every run to seconds, to check the script.

## What's here

- `mock/`: the fake upstream. OpenAI and Anthropic APIs, streamed or not; the model name picks the
  timing (`fast`, `chat`, `long`, `slow`, `flaky`, `err500`, `err429`, `hang`, `tok10`, `tok0`). It
  counts what it served and how long each request took, so the load generator can compute every
  request's gateway overhead exactly, and stamps each streamed token with its send time.
- `loadgen/`: the load generator. Fixed arrival rate (an overloaded gateway can't slow the senders
  down and hide its own stalls) or fixed concurrency; records time to first token, token gaps and
  per-chunk delay. `k6.js` reruns one scenario through k6 as a cross-check.
- `compose.yml`, `litellm.yaml`, `litellm-rust.yaml`, `otari.yml`: each gateway configured the way
  its docs recommend for production, all pointing at the mock.
- `run.sh`: setup, scenarios, and a 1 Hz CPU and memory sampler reading each container's cgroup.
- `real.sh`: compatibility checks against OpenRouter free models, LM Studio and a Claude
  subscription (needs `OPENROUTER_API_KEY` in `.env`).

## Fairness rules

- Load only hits the mock. Overhead is what the client saw minus the mock's own time, per request.
- One gateway at a time, each on its own fresh Postgres, everything on one podman network.
- Same CPU quota for every gateway. The headline is 1 vCPU: Otari runs one process by design, so
  per core is the fair comparison. Scaling runs at 2 and 4 vCPU, with LiteLLM's workers set to match.
- Each gateway keeps its usage tracking on. Paths a gateway doesn't implement read n/a, not errors.
