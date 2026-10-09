# Benchmarks

llm-router 0.1.2 against LiteLLM's Python proxy (v1.104.0), LiteLLM's new Rust gateway (built from
commit `7b439d9`) and Mozilla.ai's Otari (0.15.0), each in front of the same mock upstream on one
machine, measured 7 to 9 October 2026. The harness is in [`bench/`](../bench/), and every number
here comes from [`bench/results/2026-10-07`](../bench/results/2026-10-07/).

## Summary

- **Against LiteLLM's Python proxy and Otari**, per core, llm-router serves 120 to 150 times as many
  requests (15,000 req/s on 1 vCPU against 125 and 100). It adds 35 to 75 times less latency to a
  request (0.12 ms against 4.2 and 9.1 ms), uses 11 to 17 times less CPU per request and 20 to 35
  times less memory at idle. At 50 streams/s on 1 vCPU, LiteLLM and Otari fall seconds behind, while
  llm-router adds about a millisecond.
- **Against LiteLLM's Rust gateway, Rust is ahead on 1 vCPU, and llm-router is ahead on 4 vCPU.**
  - **On 1 vCPU,** Rust reaches 20,000 req/s (llm-router 15,000) and uses about 40% less CPU per
    request. It also idles in 2 MiB and handles 1 MB prompts in a third of the time.
  - **With more cores, Rust stops scaling.** It stays at 20,000 req/s at 2 and 4 vCPU, and its CPU
    per request rises 1.6 to 3 times. llm-router reaches 27,500 req/s on 4 vCPU.
  - **At 20,000 req/s on 4 vCPU,** llm-router uses less CPU per request than Rust (0.066 against
    0.092 ms) and has a lower p99 (0.89 against 4.2 ms).
  - **One request at a time,** added latency is close: 0.10 ms for Rust, 0.12 ms for llm-router.

  The Rust gateway is an early beta that does less per request. It has master-key auth only, and
  no usage logging, budgets, rate limits, fallbacks, retries or timeouts. It also has no OpenAI-format
  streaming. Closing the 1 vCPU gap is the [next piece of work](#whats-next-for-llm-router).
- **Where llm-router falls short:**
  - There's no upstream timeout, so a hung upstream never fails over. LiteLLM and Otari fail over
    after 5 s.
  - A 429's `Retry-After` isn't passed on. Only Otari passes it on.
  - Rate limits use fixed one-minute windows, so a burst that crosses a minute boundary gets up to
    twice the limit through.
  - 1 MB prompts take 8.7 ms because the body is decoded and re-encoded.
- **Usage accounting is exact for all three gateways that record it.** In every run where the client
  got all its responses, the usage rows and token sums match the mock's own counts.

## Results

1 vCPU per gateway unless a row says otherwise. Bold marks a failure.

<!-- generated: go run ./loadgen summary results/2026-10-07 (in bench/) -->
| | llm-router | LiteLLM Rust | LiteLLM | Otari |
|---|---|---|---|---|
| Image, download size | 6 MB | 133 MB unpacked | 409 MB | 213 MB |
| Restart to first proxied request | 0.7 s | 0.2 s | 4.7 s | 3.1 s |
| Idle memory | 16 MiB | 2 MiB | 541 MiB | 337 MiB |
| Max sustainable req/s, 1 vCPU | 15000 | 20000 | 125 | 100 |
| Max sustainable req/s, 2 vCPU | 15000 | 20000 | 325 | 71 |
| Max sustainable req/s, 4 vCPU | 27500 | 20000 | 500 | 71 |
| Added latency p50 / p99, 100 req/s | 0.75 / 1.31 ms | 0.62 / 1.13 ms | 3.62 / 14.99 ms | 9.25 / 21.46 ms |
| Added latency, one request at a time | 0.12 ms | 0.10 ms | 4.20 ms | 9.07 ms |
| CPU per request, 100 req/s | 0.44 ms | 0.26 ms | 4.95 ms | 7.42 ms |
| Postgres CPU per request, 100 req/s | 0.11 ms | n/a | 0.16 ms | 0.55 ms |
| Streaming: added time to first token, 10 streams/s | 1.01 ms | n/a | 8.76 ms | 13.07 ms |
| Streaming: chunk delay p99, 50 streams/s | 1.19 ms | n/a | 2244.39 ms | 3000.11 ms |
| Claude Code request (60 KB, streamed): added time to first token, 10/s | 2.93 ms | 2.46 ms | 7.85 ms | 21.70 ms |
| Memory with 1,000 streams open (peak) | 120 MiB | 105 MiB | overloaded | 1715 MiB |
| Usage rows and tokens match the upstream's own counts | yes, exactly, in all 32 runs | n/a (no usage logging) | yes, exactly, in all 27 runs | yes, exactly, in all 27 runs |
| Client hangs up mid-stream: upstream call stopped | yes | yes | yes | **no**, 200 of 200 still open 2 s later |
| Client hangs up before the first byte: upstream call stopped | yes | yes | **no**, 200 of 200 still open 2 s later | **no**, 200 of 200 still open 2 s later |
| Fails over from a 500, a 429 and a flaky upstream | yes | n/a (no fallbacks) | yes | yes |
| Fails over from a hung upstream | **no** (no upstream timeout) | n/a (no fallbacks) | yes, after 5.0 s | yes, after 5.1 s |
| Passes the upstream's Retry-After on a 429 | no | no | renamed, so clients miss it | yes |
| 6,000 requests waiting 30 s on a slow upstream | 12000 of 12000 ok | 12000 of 12000 ok | 583 of 12000 ok | 991 of 12000 ok |
| Soak, 2 vCPU, 70% plain + 30% streamed: requests ok | 10200 of 10200 | 10200 of 10200 | 10200 of 10200 | 10200 of 10200 |
| Soak: memory, first minute → last minute | 25 → 35 MiB | 6 → 6 MiB | 1632 → 1693 MiB | 523 → 684 MiB |
| Spike to twice its own 2 vCPU maximum, 30 s: requests ok | 20% | 12% | 81% | 23% |
| Back to normal after the spike | within 10 s | within 10 s | within 20 s | within 20 s |

### Non-streaming ladder, 1 vCPU (added latency p50 / p99 ms)

| req/s | direct | llm-router | LiteLLM Rust | LiteLLM | Otari |
|---|---|---|---|---|---|
| 10 | 0.73 / 0.97 | 1.49 / 1.94 | 1.34 / 1.71 | 13.23 / 19.21 | 20.41 / 30.53 |
| 25 | 0.62 / 0.84 | 1.32 / 1.76 | 1.24 / 1.54 | 6.27 / 10.38 | 12.25 / 16.47 |
| 50 | 0.47 / 0.77 | 1.02 / 1.59 | 0.95 / 1.39 | 4.61 / 9.98 | 10.06 / 13.68 |
| 100 | 0.31 / 0.55 | 0.75 / 1.22 | 0.63 / 1.08 | 3.64 / 14.97 | 9.22 / 16.08 |
| 106 | | | | | 16.35 / 88.45 |
| 112 | | | | | overloaded |
| 125 | | | | 3.44 / 29.01 | overloaded |
| 137 | | | | 3.34 / 50.04 | |
| 150 | | | | 3.25 / 50.44 | overloaded |
| 200 | 0.25 / 0.45 | 0.64 / 1.04 | 0.50 / 0.92 | 3.01 / 141.68 | overloaded |
| 300 | 0.23 / 0.41 | 0.58 / 0.93 | 0.46 / 0.73 | overloaded | |
| 500 | 0.18 / 0.36 | 0.35 / 0.93 | 0.34 / 0.67 | | |
| 750 | 0.12 / 0.31 | 0.22 / 0.35 | 0.15 / 0.21 | | |
| 1000 | 0.14 / 0.36 | 0.23 / 0.36 | 0.24 / 0.43 | | |
| 1500 | 0.15 / 0.39 | 0.16 / 0.26 | 0.13 / 0.26 | | |
| 2000 | 0.15 / 0.41 | 0.15 / 0.27 | 0.13 / 0.24 | | |
| 3000 | 0.13 / 0.46 | 0.15 / 0.32 | 0.15 / 0.23 | | |
| 5000 | 0.11 / 0.47 | 0.18 / 0.39 | 0.21 / 0.33 | | |
| 7500 | 0.09 / 0.26 | 0.14 / 0.39 | 0.15 / 0.44 | | |
| 10000 | 0.08 / 0.29 | 0.14 / 0.34 | 0.13 / 0.54 | | |
| 15000 | 0.08 / 0.32 | 0.14 / 0.55 | 0.12 / 0.19 | | |
| 16250 | | overloaded | | | |
| 17500 | | overloaded | | | |
| 20000 | 0.06 / 1.01 | overloaded | 0.11 / 1.48 | | |
| 21250 | | | overloaded | | |
| 22500 | | | overloaded | | |
| 25000 | | | overloaded | | |
| 30000 | 0.06 / 7.24 | | overloaded | | |
| 32500 | 0.06 / 13.12 | | | | |
| 35000 | 0.06 / 24.60 | | | | |
| 40000 | overloaded | | | | |

### Fixed concurrency (added latency p50 / p99 ms, req/s)

| test | direct | llm-router | LiteLLM Rust | LiteLLM | Otari |
|---|---|---|---|---|---|
| fast-1 | 0.05 / 0.07, 17818/s | 0.12 / 0.16, 7815/s | 0.10 / 0.13, 8970/s | 4.20 / 17.48, 218/s | 9.07 / 12.20, 106/s |
| fast-10 | 0.05 / 0.15, 163640/s | 0.18 / 0.70, 28845/s | 0.27 / 0.38, 37278/s | 33.21 / 86.10, 260/s | 82.37 / 105.04, 118/s |
| fast-50 | 0.23 / 0.81, 188448/s | 0.86 / 38.44, 30030/s | 1.35 / 1.76, 37904/s | 186.53 / 455.87, 241/s | 454.88 / 685.44, 107/s |
| fast-100 | 0.45 / 1.69, 191565/s | 1.81 / 42.84, 29050/s | 2.72 / 3.05, 36914/s | 410.39 / 647.26, 236/s | 894.75 / 1591.19, 107/s |
| tok10-1 | ttft +0.46, delay p99 0.33, batched 0%, 2/s | ttft +0.98, delay p99 0.53, batched 0%, 2/s | n/a | ttft +13.55, delay p99 3.77, batched 0%, 2/s | ttft +16.43, delay p99 1.61, batched 0%, 2/s |
| tok10-10 | ttft +0.29, delay p99 0.33, batched 0%, 20/s | ttft +0.99, delay p99 0.72, batched 0%, 20/s | n/a | ttft +12.77, delay p99 105.99, batched 2%, 19/s | ttft +15.38, delay p99 6.18, batched 0%, 19/s |
| tok10-50 | ttft +0.46, delay p99 0.90, batched 0%, 100/s | ttft +1.67, delay p99 2.34, batched 0%, 100/s | n/a | ttft +211.85, delay p99 428.03, batched 27%, 63/s | ttft +173.57, delay p99 247.67, batched 0%, 66/s |
| tok0-1 | ttft +0.05, delay p99 0.03, batched 100%, 6548/s | ttft +0.12, delay p99 0.22, batched 100%, 2601/s | n/a | ttft +10.98, delay p99 12.68, batched 100%, 74/s | ttft +7.50, delay p99 4.89, batched 100%, 67/s |
| tok0-10 | ttft +0.38, delay p99 0.41, batched 100%, 19211/s | ttft +1.07, delay p99 16.18, batched 100%, 4992/s | n/a | ttft +90.82, delay p99 86.24, batched 100%, 72/s | ttft +59.73, delay p99 88.93, batched 55%, 68/s |
| tok0-50 | ttft +2.17, delay p99 1.85, batched 100%, 19434/s | ttft +6.64, delay p99 35.24, batched 100%, 4974/s | n/a | ttft +494.57, delay p99 582.21, batched 100%, 68/s | ttft +334.36, delay p99 556.82, batched 0%, 62/s |
<!-- end generated -->

**Terms used in these tables:**

- **Added latency** is what the client measured, minus the time the mock spent on the request. Each
  request is matched to the mock's own record by an ID in the prompt. It includes the extra network
  hop through the gateway. The direct column (load generator straight to the mock) shows that
  baseline, about 0.3 ms at 100 req/s.
- **Max sustainable req/s** is the highest fixed arrival rate at which all of these hold:
  - p99 added latency stays at or under 50 ms;
  - at most 0.1% of requests fail;
  - the full rate is reached, and the sender keeps up.

  Requests start on schedule whether or not earlier ones have finished, so a stalled gateway can't
  slow the senders down and hide the stall. The rate steps up until something breaks, then bisects
  to within 10%.
- **CPU per request** is the gateway container's CPU time over the measured window, divided by
  requests. At low rates, idle background work inflates it, so compare gateways at the same rate.
- **n/a** means the gateway doesn't implement that path. **overloaded** means the run failed: more
  than 1% errors, or under 95% of the target rate.

## Throughput and CPU scaling

Max sustainable req/s, non-streaming, 2 KB prompts:

| vCPU | llm-router | LiteLLM Rust | LiteLLM | Otari |
|---|---|---|---|---|
| 1 | 15,000 | 20,000 | 125 | 100 |
| 2 | 15,000 | 20,000 | 325 | 71 |
| 4 | 27,500 | 20,000 | 500 | 71 |

LiteLLM runs one worker per vCPU. Otari runs one process at every size, because it refuses more
workers, so extra vCPUs don't help it.

CPU per request at the same rate, in ms (p99 added latency in brackets):

| req/s | llm-router 1 vCPU | Rust 1 vCPU | llm-router 4 vCPU | Rust 4 vCPU |
|---|---|---|---|---|
| 5,000 | 0.053 (0.39) | 0.026 (0.33) | 0.072 (0.47) | 0.067 (0.47) |
| 10,000 | 0.067 (0.34) | 0.037 (0.54) | 0.074 (0.35) | 0.054 (0.28) |
| 15,000 | 0.060 (0.55) | 0.034 (0.19) | 0.069 (0.43) | 0.054 (0.33) |
| 20,000 | overloaded | 0.029 (1.48) | 0.066 (0.89) | 0.092 (4.22) |

On one core, Rust does the same work in a little over half the CPU. Given more cores, Rust spends
more CPU per request while llm-router's cost stays roughly flat.

**Neither fast gateway's limit is its CPU allowance:**
- Rust stops at 20,000 req/s at every size, using 0.6 cores on 1 vCPU and 2 to 2.5 cores on 4 vCPU.
- llm-router at 2 vCPU collapses at 16,250 req/s while using 1.0 to 1.5 of its 2 cores.

The setup isn't the limit either: llm-router carries 27,500 req/s through it on 4 vCPU, and the
direct path tops out at 32,500.

When llm-router does fall behind, it opens a new upstream connection for nearly every request:
92,000 in one 20 s step. That deepens the collapse instead of recovering from it.

## Fixed concurrency

A friend's independent study compared LiteLLM and llm-router with N clients, each sending its next
request as soon as the last one returns. The fixed-concurrency table above repeats that setup for
three upstream profiles:
- `fast`: an instant, non-streamed reply.
- `tok10`: 50 tokens, 10 ms apart.
- `tok0`: 50 tokens, all at once.

Past saturation, latency in this mode is just concurrency divided by throughput, so read the req/s.

llm-router's p99 at 50 to 100 clients is 38 to 43 ms, against under 3 ms for Rust. That's its tail
at saturation, which also shows at 25,000 req/s and above on 4 vCPU (p99 12 to 13 ms).

## Streaming

The `chat` profile streams a 300 ms first token, then 100 chunks 20 ms apart (2.3 s in all), through
the OpenAI API. LiteLLM Rust can't stream that API yet. Cells show added time to first token
p50 / p99, then chunk delay p99, all in ms.

| streams/s, 1 vCPU | direct | llm-router | LiteLLM | Otari |
|---|---|---|---|---|
| 10 | 0.49 / 0.85, 0.30 | 1.01 / 1.45, 0.70 | 8.76 / 333, 188 | 13.1 / 70.8, 4.4 |
| 50 | 0.36 / 1.06, 0.29 | 0.89 / 2.20, 1.19 | 3,799 / 18,680, 2,244 | 720 / 1,791, 3,000 |
| 200 | 0.21 / 0.94, 0.28 | 0.46 / 1.46, 0.90 | **overloaded**, 1,421 of 6,000 ok | **overloaded**, 298 of 6,000 ok |
| 500 | 0.35 / 1.34, 0.24 | 0.61 / 6.70, 2.67 | – | – |
| 1,000 | 0.51 / 2.07, 0.15 | 0.71 / 41.6, 27.0 | – | – |

At 1,000 streams/s, about 2,300 streams are open at once on one vCPU.

Peak memory with 30 s Anthropic streams held open, 1 vCPU:

| streams open | llm-router | LiteLLM Rust | LiteLLM | Otari |
|---|---|---|---|---|
| 1,000 | 120 MiB | 105 MiB | **overloaded**, 420 of 1,080 ok | 1,715 MiB |
| 5,000 | 628 MiB | 537 MiB | – | **overloaded**, 225 of 5,490 ok |
| 10,000 | 1,519 MiB | 1,117 MiB | – | – |

## Request size, TLS, Anthropic and translation

Cells show added latency p50 / p99 in ms. CPU per request is in brackets where it's comparable.

| test | direct | llm-router | LiteLLM Rust | LiteLLM | Otari |
|---|---|---|---|---|---|
| 2 KB prompt, 20 req/s | 0.65 / 0.89 | 1.53 / 1.88 (0.8) | 1.67 / 2.02 (0.8) | 7.90 / 12.6 (9.9) | 13.3 / 178 (11.3) |
| 100 KB prompt, 20 req/s | 0.71 / 1.03 | 2.37 / 3.82 (2.0) | 1.93 / 2.37 (1.1) | 8.08 / 12.9 (10.2) | 13.9 / 18.7 (11.3) |
| 1 MB prompt, 20 req/s | 0.82 / 1.69 | **8.65 / 12.2** (8.8) | 3.22 / 4.19 (2.4) | 9.17 / 16.5 (11.2) | 17.6 / 21.6 (15.5) |
| HTTPS upstream, 50 req/s | 0.55 / 0.86 | 1.23 / 1.88 | 1.12 / 1.87 | 5.08 / 15.1 | 11.6 / 520 |
| Claude Code request (60 KB, streamed), 10/s: time to first token | 0.72 / 1.29 | 2.93 / 3.96 | 2.46 / 3.96 | 7.85 / 19.8 | 21.7 / 229 |
| same, 50/s | 0.56 / 1.27 | 2.12 / 3.91 | 1.59 / 4.02 | 8.28 / 345 | **2,287 / 19,787** |
| OpenAI client, Anthropic upstream (streamed), 10/s: time to first token | – | 1.08 / 1.71 | n/a | 9.56 / 241 | 13.5 / 39.3 |
| Anthropic client, OpenAI upstream (streamed), 10/s: time to first token | – | 1.04 / 1.60 | n/a | 11.9 / 265 | 13.4 / 39.8 |
| Non-streamed 2.3 s replies, 100 req/s (230 open) | 0.34 / 0.62 | 0.81 / 1.31 | 0.75 / 1.27 | 3.85 / 205 | 393 / 2,039 |
| same, 1,000 req/s (2,300 open) | 0.16 / 0.35 | 0.22 / 0.44 | 0.21 / 0.60 | – | – |

## Resilience

The headline table covers these four checks: hang-ups, failover, `Retry-After` and a slow upstream.

In the slow-upstream test, 6,000 requests each wait 30 s for their first byte. Peak memory during
it:

| llm-router | LiteLLM Rust | LiteLLM | Otari |
|---|---|---|---|
| 573 MiB | 711 MiB | 2,015 MiB | 4,137 MiB |

LiteLLM and Otari time most of those requests out; llm-router and Rust serve all 12,000.

## Budgets, limits and usage logging

Each gateway ran at half its own 1 vCPU maximum, so compare within a column. LiteLLM Rust has none
of these features.

| | llm-router | LiteLLM | Otari |
|---|---|---|---|
| Rate | 7,500 req/s | 62 req/s | 50 req/s |
| Added latency p50, usage logging on (default) | 0.146 ms | 4.85 ms | 9.63 ms |
| … plus a budget and RPM/TPM limits on the key | 0.142 ms | 4.96 ms | **12.8 ms** |
| … with usage logging off | not tested | 5.04 ms | not tested |
| Postgres CPU per request, default → with limits | 0.008 → 0.008 ms | 0.215 → 0.217 ms | 0.692 → 1.061 ms |
| 600/min limit, 1,200 sent in a minute: let through | **930** (fixed minute windows) | 600 | 600 |
| $0.01 budget, about 14 requests' worth: let through | 18, then 429 | 18, then 422 | 13, then 403 |

## Soak and spike

Every gateway ran the same absolute load on 2 vCPU: 35 req/s for 5 minutes. That's half
the slowest gateway's 2 vCPU maximum (Otari's 71). 70% of requests were plain and 30% streamed
Claude Code-style. Each gateway then took a 30 s spike at twice its own maximum, then 10 s slices at
the soak rate until it served them with no errors and p99 under 50 ms. The rows are in the headline
table.

- **Errors:** all four served every soak request.
- **Memory growth over the 5 minutes:**
  - Otari: 161 MiB (523 → 684 MiB);
  - LiteLLM: 61 MiB;
  - llm-router: 10 MiB;
  - Rust: none.
- **The spike:** each gateway's spike rate is twice its own maximum: llm-router 30,000, Rust
  40,000, LiteLLM 650 and Otari 142 req/s. So the share served during the spike doesn't compare
  across columns. The time to recover does: 10 s for llm-router and Rust, 20 s for LiteLLM and
  Otari.

A 5-minute soak catches errors under steady load and fast leaks, but not slow growth.

## Features

Only what this benchmark exercised:

| | llm-router | LiteLLM Rust | LiteLLM | Otari |
|---|---|---|---|---|
| Language | Go | Rust | Python | Python |
| Process model | one process, all cores | one process, all cores | one worker process per core | one process, one core |
| Needs | Postgres | a config file | Postgres | Postgres |
| Client auth | virtual keys | master key only | virtual keys | virtual keys |
| Usage and cost per request | yes | no | yes, written in 60 s batches | yes |
| Budgets, RPM and TPM limits | yes | no | yes | yes |
| OpenAI chat, streamed | yes | no (501) | yes | yes |
| Anthropic `/v1/messages` | yes | to Anthropic upstreams only | yes | yes |
| OpenAI ↔ Anthropic translation | yes | no | yes | yes |
| Failover across deployments | yes | no | yes | yes |
| Upstream timeout | no | no | yes | yes |
| Passes on `Retry-After` | no | no | renamed | yes |
| Stops the upstream call when the client hangs up | yes | yes | only once streaming has started | no |

## Setup

- **Machine:**
  - Apple M5 Pro (15 cores, 24 GB), macOS 27.0.1.
  - podman 6.1.3, with a podman machine on Fedora (kernel 6.15.10) given 10 vCPU and 16 GB.
- **Network:** the load generator, mock, gateway and Postgres all run inside that VM on one podman
  network, so traffic never crosses macOS port forwarding. Only one gateway runs at a time, each
  with a fresh Postgres 17.
- **CPU and memory limits** are quotas (`cpus:`), the way Kubernetes limits work:
  - gateway: 1, 2 or 4 vCPU;
  - mock: 2 vCPU;
  - load generator: 2 vCPU;
  - Postgres: 2 vCPU;
  - memory: 8 GB per gateway.
- **Images:**
  - `ghcr.io/exitcodenihil/llm-router:0.1.2`
  - `ghcr.io/berriai/litellm:v1.104.0`
  - `docker.io/mzdotai/otari:0.15.0`
  - LiteLLM Rust built from `BerriAI/litellm@7b439d9`; no image is published yet.

  Digests are in `bench/results/2026-10-07/image-*.txt`.
- **Configuration:** each gateway is set up the way its docs recommend for production:
  - a database;
  - a virtual key, except for Rust;
  - usage tracking on;
  - no response cache;
  - quiet logs.

  See [`bench/litellm.yaml`](../bench/litellm.yaml), [`otari.yml`](../bench/otari.yml) and
  [`litellm-rust.yaml`](../bench/litellm-rust.yaml). llm-router is set up through its API
  ([`run.sh`](../bench/run.sh)).
- **Tools:** the mock and load generator are small Go programs, described in
  [`bench/README.md`](../bench/README.md).
- **Checks on the harness itself:**
  - CPU sampling reads 0.98 cores for a 1-vCPU busy loop, and 264 MiB for a 256 MiB allocation.
  - k6 agrees with the load generator to within 10%: p50 0.218 / p99 0.387 ms against 0.234 / 0.410
    ms, through llm-router at 1,000 req/s.

## Caveats

- **This is a laptop VM, not a server.** Absolute numbers will differ on other hardware; the ratios
  are the point.
- **The headline is per core,** because Otari runs one process by design.
- **LiteLLM writes spend in 60 s batches,** as recommended, so its Postgres cost lands outside short
  windows. The usage checks wait for the flush.
- **llm-router logs one line per request,** and that can't be turned off. Logging is discarded here
  (driver `none`), so the write costs CPU but nothing stores it.
- **Client to gateway is plain HTTP.** TLS to the upstream is measured separately, in the HTTPS
  upstream row.
- **LiteLLM Rust is an early beta.** It will gain features, and probably cost per request with them.
- **The 2 and 4 vCPU runs for Otari and Rust, and the soak, ran on 9 October.** The first attempt
  was discarded because the Mac went to sleep during it.
- **Real providers haven't been checked yet:** OpenRouter, LM Studio and a Claude subscription.
  The check is `./run.sh real`.

## What's next for llm-router

From these numbers, in order:

1. **An upstream timeout,** so a hung upstream fails over, and passing on `Retry-After`.
2. **Sliding-window rate limits.**
3. **Pass the request body through** instead of decoding and re-encoding it. 1 MB prompts take
   8.7 ms, against 3.2 ms for Rust.
4. **Fewer allocations and a cheaper usage parse per request,** to close the CPU gap with Rust
   (0.060 against 0.034 ms per request at 15,000 req/s) and the p99 tail at saturation.
5. **Keep upstream connections when falling behind,** instead of opening one per request.

To reproduce, see [`bench/README.md`](../bench/README.md).
