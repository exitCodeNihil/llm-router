# Configuration

## Environment / flags

| Env | Flag | Mode | Description |
|---|---|---|---|
| `LLMR_MODE` | `--mode` | | `all` (default; control plane + gateway) or `gateway` (edge node) |
| `LLMR_LISTEN` | `--listen` | | Listen address, default `:8080` |
| `LLMR_DATABASE_URL` | `--database-url` | all | Postgres URL (required) |
| `LLMR_ADMIN_TOKEN` | | all | Bootstrap admin bearer token for the API/console before SSO is set up (optional; at least 12 characters, placeholders such as `change-me` are refused) |
| `LLMR_ENCRYPTION_KEY` | | all | Key for encrypting provider secrets at rest and signing sessions (required; at least 16 characters; placeholders refused; rotate = re-enter provider keys and every session signs out) |
| `LLMR_IDE_LISTEN` | `--ide-listen` | all | Second listen address that serves only workspace IDEs, e.g. `:8081`. Set with `LLMR_IDE_ORIGIN`. |
| `LLMR_COOKIE_DOMAIN` | `--cookie-domain` | all | `Domain` attribute for the session cookie, e.g. `.example.com`. Needed only when `LLMR_IDE_ORIGIN` is a different hostname (`ide.example.com`) rather than the console's host on another port; the cookie must reach the IDE origin. |
| `LLMR_IDE_ORIGIN` | `--ide-origin` | all | Origin the browser reaches that listener at, e.g. `https://ide.example.com` or `http://localhost:8081`. Without it the IDE shares the console's origin, and a page rendered inside a workspace (a dev server opened through code-server's port proxy, an extension) could call the console API as you. Set it in production. |
| `LLMR_SEED_CLAUDE_SUBSCRIPTION` | | all | `1` registers the `anthropic-subscription` pass-through provider and Claude model names on startup (idempotent, stores no credential) |
| `LLMR_REDIS_URL` | | all | Reserved for multi-node exact rate limiting (not yet used) |
| `LLMR_CONTROL_PLANE_URL` | `--control-plane-url` | gateway | Control plane base URL (required) |
| `LLMR_NODE_TOKEN` | | gateway | Edge node token from the console (required) |
| `LLMR_SNAPSHOT_CACHE` | `--snapshot-cache` | gateway | Disk cache path, default `/var/lib/llmrouter/snapshot.json` |

## Who may call which model

Every model the gateway serves is callable unless a policy narrows it. Policies live on
teams, users and keys:

| Level | Set by | What it does |
|---|---|---|
| Team `allowed_models` | admin | What the team's keys may call. Also feeds each member's default access. |
| User `allowed_models` | admin | Overrides the team default for that person, everywhere they act. |
| Key `allowed_models` | admin, team admin, or the key's owner | Narrows one key further. |

What a person may call by default is the **union of their teams' policies** (a team with
no policy allows everything). A user policy replaces that union. A request through a key
must pass the key's own list, the user's effective policy and, for a team key, that
team's policy. `/v1/models` on a key returns exactly what it may call; the **Models** page
shows each signed-in user what their account may use, with list prices and live status.

### Keys, and who pays

Creating a key asks one thing: **who it spends against**.

- **Just me** — a personal key. Counts against the user's budget and rate limits; gets the
  user's effective models.
- **A team I'm in** — any member can mint one for a team they belong to. It counts against
  the team *and* against the member (both budgets, both rate limits) and uses the team's
  models. The member manages their own team keys; a team admin manages all of the team's.
- **Another user** — admins only.

Budgets stack. A request through a key must be within every ceiling that applies:

| Budget | Where | Caps |
|---|---|---|
| Key | API Keys → Edit (admin or team admin) | that one key |
| Member share | Teams → Members → the budget button next to a member (team admin) | what that member spends through *that team's* keys |
| Team | Teams → Edit (admin) | everything spent through the team's keys, all members combined |
| User | Users → Edit (admin) | everything that person spends, personal and team keys alike |

A member cannot set any of these on themselves, because a cap you can lift is no cap.

## Providers

- `azure`: `base_url` is the resource endpoint — either `https://<resource>.openai.azure.com`
  or an Azure AI Foundry endpoint `https://<resource>.services.ai.azure.com`; both serve the
  same `/openai/deployments/...` surface the gateway uses.
  `auth_mode: entra` uses DefaultAzureCredential (managed identity / workload identity /
  `az login`); `auth_mode: api_key` stores the key encrypted. Optional `config.api_version`
  (default `2024-10-21`). Requests go to `/openai/deployments/<upstream_name>/...`.
- `gcp_vertex`: Google Cloud Vertex AI. `config.project` is the project id and
  `config.location` the region (`global` by default; Claude models need a real region such
  as `us-east5`). `base_url` may be left blank — the console fills in
  `https://<location>-aiplatform.googleapis.com` — and only needs setting for a private
  endpoint. `auth_mode: gcp_adc` uses Application Default Credentials (the attached service
  account on GCE/GKE, workload identity, or `gcloud auth application-default login` on the
  gateway host); `gcp_sa` stores a pasted service-account JSON key encrypted and mints
  tokens with it. Gemini and every Model Garden MaaS model (Qwen, GLM, Llama, DeepSeek, gpt-oss…)
  go through Vertex's OpenAI-compatible endpoint with the publisher-prefixed upstream name
  (`google/gemini-2.5-flash`, `qwen/qwen3-next-80b-a3b-instruct-maas`); a model that
  returns "not found" needs enabling in Model Garden for the project first. Chat only:
  Vertex's endpoint refuses `/embeddings`, so embeddings stay on another provider. Claude goes
  through `publishers/anthropic/models/<id>:rawPredict` — see below.
- `openrouter`: [OpenRouter](https://openrouter.ai). `auth_mode: bearer` with an OpenRouter
  key; `base_url` may be left blank (`https://openrouter.ai/api/v1`). Upstream names are
  OpenRouter slugs (`anthropic/claude-sonnet-4-5`, `nvidia/nemotron-3.5-lightning:free`).
  Discovery returns OpenRouter's whole catalogue with its published per-token prices and
  context length, and the console turns those into the model's custom price — so a free
  model is recorded at $0 (priced, not *unpriced*) and a paid one at OpenRouter's rate.
- `openai_compatible`: any OpenAI-style server — vLLM, LM Studio, TGI, Ollama, OpenAI
  itself. `base_url` up to and including `/v1` (e.g. `http://vllm:8000/v1`,
  LM Studio `http://localhost:1234/v1`). `auth_mode: bearer` (stored encrypted) or `none`.
  `auth_mode: oauth_passthrough` stores no credential at all: the caller's own
  `Authorization` header is forwarded upstream, and clients send their gateway key as
  `X-Llmr-Key` instead. Built for Claude Code on a Claude subscription — see
  [integrations](integrations.md#claude-code-on-a-claude-subscription). Model discovery
  does not work for these providers (there is no credential to list with); type the
  upstream name into the deployment form.

The console's Models page (a deployment in the API) discovers what a provider serves (`GET
/api/providers/{id}/models`): Azure lists your deployments (deployment name + underlying
model, with catalog pricing pre-filled), Vertex lists the Google and Anthropic publisher
catalogues (the other MaaS publishers return nothing from that API; type those by hand),
OpenAI-compatible servers list their loaded models.

## Model deployments

A deployment (a *model* in the console) maps the public `model_name` clients send to an
`upstream_name` on one provider. Multiple deployments may share a `model_name` — the
same model on Vertex, Foundry and OpenRouter, say — and the gateway tries them in this
order:

1. `priority`, lowest first;
2. within a priority, **cheapest first** by list price (input + output per 1M tokens, from
   the deployment's custom price or its catalog entry); unpriced backends go last;
3. then by upstream name, so the order is stable.

So leaving every priority at 0 already routes to the cheapest backend and falls over to
the others. Failover happens on connection errors, 429 and 5xx, only before any byte has
been streamed to the client. A backend that fails is then **held out of routing** for a
cooldown — 30 s, doubling per consecutive failure up to 5 min, or the upstream's
`Retry-After` if longer — so a dead endpoint does not cost every request a connect
timeout, and a throttled one is not hammered until its window resets. It is tried again
when the cooldown lapses (or immediately if nothing healthier is left). The Models page
shows backends grouped by name in routing order, with a *cooling* badge on any that are
held out; `GET /api/routing/health` returns the same list.

Optional `context_tokens` records the deployment's context window. The gateway does
not enforce it; the console uses it to show how full a playground conversation is.

Pricing per deployment, first match wins:
1. `input_per_1m` / `output_per_1m` set → custom pricing (vLLM, on-prem), with an
   optional `cached_input_per_1m` for upstreams that discount cache reads.
2. `catalog_model_id` set (e.g. `azure/gpt-4o-2024-08-06`) → the price catalog, seeded with
   Azure list prices and editable in the console.
3. Neither → requests are recorded as *unpriced* (cost 0, flagged in the dashboard).

Deployments on an `oauth_passthrough` provider are always unpriced, whatever the catalog
says: the upstream bills the caller's own plan, so a dollar figure here would be fiction.
Judge that traffic by tokens and prompt-cache hit rate on the dashboard instead.

## Budgets & rate limits

`budget_usd` (+ `budget_period`: `daily` / `monthly` / `total`, default total), `rpm_limit`,
`tpm_limit` can each be set on API keys, users, and teams; all applicable scopes are
enforced (most restrictive wins). Budget denials return OpenAI-style `429` with code
`budget_exceeded`.

## Claude models on Azure AI Foundry and Google Vertex AI

Foundry and Vertex serve Claude via the Anthropic Messages surface, not the OpenAI
deployments API. Deployments have an `api_flavor` (`openai` default / `anthropic`): when the
upstream name starts with `claude`, the gateway sets `anthropic` automatically.
OpenAI-protocol clients are translated both ways (including streaming and tool
calls); Anthropic-protocol clients (`/v1/messages`, e.g. Claude Code) pass through
verbatim for full fidelity. Auth: `api_key` (sent as `x-api-key`) works out of the
box; `entra` additionally requires the "Azure AI User" role on the resource (the
data action `Microsoft.CognitiveServices/accounts/AIServices/providers/action`).
Catalog prices ship for `azure/claude-*` models.

On Vertex the upstream name is the Model Garden id with its version suffix where one
exists (`claude-sonnet-4-5@20250929`; newer ids such as `claude-fable-5-1` have none).
The gateway moves the model into the URL and the API version into the body
(`anthropic_version: vertex-2023-10-16`) as Vertex requires, and picks `:rawPredict` or
`:streamRawPredict` from the request's `stream` flag; everything else — prompt caching,
thinking, tool calls, the `anthropic-beta` header — passes through unchanged, so Claude
Code, Cursor and OpenCode work exactly as they do against Foundry. Catalog prices ship
for `gcp/claude-*` and `gcp/gemini-*`.

## Response headers

Every `/v1/chat/completions` response carries routing attribution, set before the
first streamed byte so it is available even for SSE:

| Header | Meaning |
|---|---|
| `X-Request-Id` | Correlates with the row in the request log / `usage_events` |
| `X-Llmr-Provider` | Which provider actually served the request |
| `X-Llmr-Upstream` | The upstream model name behind the public alias |
| `X-Llmr-Attempts` | `1` normally; higher when failover was used |

They are listed in `Access-Control-Expose-Headers`, so browser clients on another
origin can read them too.

## Observability export

Capture policy is edited as a draft and applied explicitly: it decides whether
prompts and completions leave the gateway, so a mis-click is a data-egress change
rather than a display preference. The console lists every pending change and
estimates the requests/min affected before you apply.

Modes:

| Mode | Behaviour |
|---|---|
| `everything` | Every request is exported; `capture_content` decides whether prompts and completions ride along |
| `by_rule` | First matching enabled rule wins, scoped to a team, user, API key or tag, each with its own capture level and sample rate. **Traffic matching no rule is not exported** |
| `off` | Nothing is exported |

Two failure modes are worth knowing, because both look like "export is broken":

- **`by_rule` with no rules exports nothing.** The console warns when this is the
  case.
- **Wrong credentials are rejected per batch, not at save time.** The console
  reports delivery health (delivered, dropped, last error), because the live
  event feed lists events *selected* for export and looks identical whether the
  destination accepted them or rejected every one. `Test connection` validates
  the key pair against the ingestion endpoint, not just host reachability.

## Workspaces

A workspace is a container you own running **VS Code** (code-server) plus a chat powered by
[pi](https://pi.dev). The editor, file explorer, search, git, debugger and terminal are all
VS Code's — including a real PTY, so ctrl+C and `vim` behave normally. The gateway proxies
the IDE behind your session; code-server itself has no password and is never exposed.
Develop → Workspace settings (admins):

| Field | Meaning |
|---|---|
| `enabled` | Master switch. Off by default; while off the workspace routes 404 |
| `runtime` | `docker` (podman serves the same API) or `kubernetes` |
| `socket` | Docker only. The runtime's socket — `/run/user/<uid>/podman/podman.sock` rootless, `/var/run/docker.sock` for Docker. On macOS podman runs in a VM and its forwarding socket sits under a per-boot temp path, so it changes across reboots |
| `storage_class`, `disk_gb` | Kubernetes only. Sizes each workspace's PVC; blank storage class takes the cluster default |
| `gateway_url` | How a workspace reaches this gateway *from inside its container* (`http://host.containers.internal:8080`), which is not the URL your browser uses |
| `default_image` | Fallback image when no template is chosen (admins only); users pick from the templates below |
| `memory_mb`, `cpus` | Per-container caps |
| `allow` | Which users and teams may use workspaces |

**It fails closed twice.** Disabled means nobody, and an empty allow-list means admins
only — never everyone.

Being allowed to use workspaces is separate from reaching a particular one. A workspace
belongs to one user, and **admin is not a master key**: an admin may list, stop and delete
any workspace — the containment actions an operator needs — but may not read its files,
exec in it, upload to it or talk to its agent. Acting as someone else inside a live shell
is an insider-threat surface, not a support tool. Everything else 404s, the same answer a
non-existent workspace gives, so ownership cannot be probed.

The agent inside a workspace authenticates with an API key minted for that workspace, so
its spend is metered in `usage_events` and constrained by that key's `budget_usd`,
`rpm_limit` and `allowed_models` — the same machinery as any other client, no special
case. To cut off a runaway workspace, disable its key; it leaves the routing snapshot
within ~2s. Filter analytics by the key to see what a workspace has cost.

### Isolation

Everything the browser sends to the IDE goes through the gateway, which checks the
session and that you own the workspace; the caller's cookie and tokens are stripped
before the request enters the container. With `LLMR_IDE_ORIGIN` set the IDE loads from a
separate origin, so nothing rendered inside a workspace runs with the console's identity;
the console also refuses state-changing requests whose `Origin` is another host (behind a
reverse proxy that rewrites `Host`, forward the public host as `X-Forwarded-Host`). Admins
see every workspace but can only stop or delete one they do not own. Deleting a user
removes their workspace containers and volumes. Keys of a disabled user stop working.

### Kubernetes

Each workspace becomes a Pod, a PVC and a Secret, all named `llmr-ws-<id>` and labelled
`app.kubernetes.io/managed-by=llm-router`. Stopping deletes the Pod and keeps the PVC —
Kubernetes has no stopped-Pod state, and the volume surviving is what makes stop/start feel
the same as on Docker. The workspace's gateway key lives in the Secret, so recreating the
Pod does not re-mint it.

**Cluster credentials come from the environment, never from the settings row** — a client
key is root on the cluster, and the settings row is plain JSON in Postgres:

| | |
|---|---|
| `LLMR_K8S_API` | `https://127.0.0.1:6443` |
| `LLMR_K8S_TOKEN` | bearer token, **or** |
| `LLMR_K8S_CLIENT_CERT` + `LLMR_K8S_CLIENT_KEY` | PEM file paths (mTLS) |
| `LLMR_K8S_CA` | PEM file path |
| `LLMR_K8S_NAMESPACE` | defaults to the in-cluster namespace, else `default` |
| `LLMR_K8S_INSECURE=1` | skip TLS verification — development only |

Running inside the cluster, none of these are needed: the service account at
`/var/run/secrets/kubernetes.io/serviceaccount` is picked up automatically. The pod needs
RBAC for pods, pods/exec, persistentvolumeclaims and secrets in its namespace — the shipped
Helm chart does **not** grant this, so add a Role and RoleBinding.

Three things that will bite on a first run:

- **Memory and CPU are ceilings, not reservations.** Requests stay small deliberately; if
  they matched the limits, a workspace would reserve its whole allowance and fail to
  schedule on a small node.
- **Locally-built images need `IfNotPresent`**, which is what workspace pods use. Load the
  image into the cluster's runtime (`nerdctl --namespace k8s.io load -i …` on
  Rancher Desktop) or push it to a registry.
- **`gateway_url` must be reachable from inside the cluster.** `host.containers.internal`
  is a Docker/podman name and means nothing to a Pod; use a Service address, or the host's
  address on the cluster network.

### What persists on Kubernetes

Only `/workspace` is on the volume, and `HOME` is set to `/workspace/.home`. That one
choice is what keeps four things across a restart:

| | |
|---|---|
| your code | `/workspace` |
| pi's chat history | `$HOME/.pi/agent/sessions/` |
| ssh keys you upload | `$HOME/.ssh` (written 0600 — ssh refuses anything looser) |
| git config | `$HOME/.gitconfig` |
| shell history and rc | `$HOME/.zsh_history`, `$HOME/.zshrc` (seeded on first start; upload your own to replace it) |

The terminal is zsh. The model list pi sees (`$HOME/.pi/agent/models.json`) follows the
console: add or remove a model and running workspaces get the new list within a few
seconds, no restart — a pi session already open picks it up on its next launch.

**Everything outside `/workspace` is lost when the container is recreated**, including
anything you `apt install` from the workspace terminal. Add tools to the template
Dockerfile instead. Deleting a workspace deletes its volume, and with it all of the above.

### Guard rails

Each workspace gets a key with a **daily budget** (default $5) and each user a
**workspace limit** (default 5) — both settable above. They are defaults rather than
options because without them "the agent is capped" and "the host cannot be filled" would
be claims with nothing behind them. Non-admins may only create workspaces from the
default image or the configured list; a free-text image reference would let anyone make
the control plane pull arbitrary registry content.

Containers run with `--cap-drop=ALL`, `no-new-privileges`, a pids cap, and the memory and
CPU limits above. They never see the container socket.

**What is not fenced off: the network.** A workspace sits on the default bridge, so it can
reach the host's LAN, anything published on the host (including Postgres on 5433 in the
dev compose stack), and cloud metadata endpoints. The agent inside runs commands by
design, so treat a workspace as having the network reach of the host it runs on. Put the
gateway on a dedicated network, or firewall the host, if that matters to you.

### Limits worth knowing

- **Extensions come from Open VSX, not Microsoft's marketplace.** Microsoft forbids
  non-Microsoft VS Code builds from using theirs. Most things are on Open VSX; Live Share
  and the Remote extensions are closed-source and unavailable — irrelevant here, since the
  workspace already *is* the remote.
- **Browsers intercept some IDE shortcuts** (⌘W, ⌘N and friends) inside a tab, and an
  iframe adds a few more. "Open in a tab" from the workspace header helps; installing
  code-server as a PWA gets you the rest.
- One chat turn at a time per workspace; a second returns 409. pi keeps a single session
  per directory and two concurrent turns would corrupt it.
- **On Kubernetes the control plane must be on the cluster network** to reach the IDE — the
  pod's port is never published. That holds when llm-router runs in the cluster, and not
  when it runs on a laptop against a remote API server.
- A workspace image must ship `code-server`, `pi` and `tar`. Start from a template.

### Templates

Users create a workspace from a **template**: a name, an image tag and the Dockerfile
that produces it. Six ship built in — `base`, `python`, `node`, `go`, `java` and
`infra` (OpenTofu, Terragrunt, TFLint, kubectl, Helm) — and admins add or edit templates
on the Workspaces page, including the Dockerfile text. On Docker / podman the console
**builds** the image straight from that Dockerfile (streamed daemon output); on
Kubernetes build it with your CI and push the tag somewhere the cluster can pull from.
Shipped templates are refreshed on upgrade until an admin edits one, after which the
edit wins (the same rule as the price catalog). Non-admins can only create workspaces
from a template; admins may also type any image.

### Your files

Under *Your files* on the Workspaces page a user stores files that every workspace they
own receives in `$HOME` on every start — an ssh key and `.ssh/config`, `.gitconfig`,
`.npmrc`, cloud credentials. They are encrypted at rest with `LLMR_ENCRYPTION_KEY`,
capped at 1 MB each, and anything under `.ssh/` is written `0600`. One upload, every
workspace, present and future.

### What persists

`/workspace` is the volume (a Docker named volume, or a PVC on Kubernetes) and `$HOME`
is `/workspace/.home`, so everything a toolchain caches under the home directory survives
a restart *and* a container recreate by construction: `~/.npm`, `~/.m2`, `~/.gradle`,
`~/go/pkg/mod` and `~/.cache/go-build`, pip's cache, OpenTofu's plugin cache, pi's
sessions, the uploaded keys. The templates set the cache environment variables
explicitly (`GOPATH`, `GRADLE_USER_HOME`, `TF_PLUGIN_CACHE_DIR`, …) so nothing lands in
the container layer by accident. Anything outside `/workspace` — an `apt install` from
the terminal — is lost on recreate; put it in the template instead.

## Analytics

`/analytics` applies one filter set to the headline, the trend and the breakdown,
and hands the same filters to the request list, so a number and the rows behind
it always describe the same traffic.

`usage_events` records three dimensions beyond the obvious ones:

| Column | Meaning |
|---|---|
| `attempts` | `1` when the first-choice deployment answered, higher after failover |
| `error_code` | the gateway's own reason (`model_not_found`, `model_not_allowed`, `rate_limit_exceeded`, `budget_exceeded`, `upstream_rate_limited`, …) |
| `ttft_ms` | time to first streamed token; null for buffered responses |

Requests the gateway refuses are now recorded too. Previously a rate-limited or
budget-blocked request returned 429 and left no trace at all, so the failures an
operator most wants to see were the ones missing. They carry zero tokens and zero
cost. Limits are checked before the body is parsed, so those rows have no
`model_name` — filter by outcome or error reason to find them.

### Tool calling

Supported on every path, and verified round-trip (request → tool call → tool
result → final answer) against a local LM Studio model, Azure/Foundry and a
Claude subscription, buffered and streaming:

| Client sends | Upstream | Handling |
|---|---|---|
| OpenAI `tools` | OpenAI | passed through |
| OpenAI `tools` | Anthropic | converted both ways, including `input_json_delta` while streaming |
| Anthropic `tools` | Anthropic | untouched |

The gateway relays definitions and results; it never executes a tool. Execution
stays in the harness, which is why Claude Code's own tools work unchanged
through the proxy.

### Prompt caching

The gateway runs no cache of its own. It preserves the client's `cache_control`
on the Anthropic path and records cache reads as `cached_tokens`. OpenAI-format
requests cannot set `cache_control` — the format has no equivalent — so caching
needs the Anthropic surface (`/v1/messages`) or an Anthropic-flavour deployment
addressed natively.

Cache reads are billed at `cached_input_per_1m` when one is set — on the catalog
entry, or on the deployment itself under custom pricing. Leaving the deployment's
blank is not "free": it bills cache reads at the full input rate, which is right
for upstreams that do not discount them and wrong for those that do (Anthropic
charges 0.1×). On cache-heavy traffic the difference is ~7× — see
[context compression](research/context-compression.md#3-two-measurement-defects-found-while-checking-the-above),
which also explains why compressing such a prompt costs more than it saves.

### Valuing unbilled traffic

Subscription traffic (an `oauth_passthrough` provider) has no marginal cost, so
`cost_usd` is 0 and `unpriced` is true — correct, but it leaves the tokens
unvalued. `notional_cost_usd` holds what the same request would have cost at the
deployment's configured rates, so "is the subscription worth it" is answerable.
It is stored in its own column and never added to spend, budgets or invoices; the
console shows it prefixed with ≈ and in muted type.

Set `input_per_1m` / `output_per_1m` on the deployment (or point it at a catalog
entry) or the value stays 0 — no rates are assumed. Existing rows are not
rewritten, so requests recorded before the rates were set keep showing $0.0000.
On cache-heavy traffic, set `cached_input_per_1m` too (or use a catalog entry,
which already carries one) — without it every cache read is valued at the full
input rate and the figure is inflated ~7× (see the prompt-caching note above).

Anthropic's Admin API (`/v1/organizations/cost_report`) reports **API** billing
only and needs an admin key; there is no endpoint that returns a per-request cost
for subscription usage, because none exists.

When Langfuse export is configured, each request row links to its trace. The
exporter uses the gateway's request id as the Langfuse trace id, so the link is
the project's trace URL plus that id — no lookup involved.
