# Client integrations

The gateway speaks two inbound protocols on the same port: OpenAI-compatible
(`/v1/chat/completions`, `/v1/embeddings`, `/v1/models`) and Anthropic Messages
(`/v1/messages`) — so both OpenAI-style and Anthropic-style clients work with a
gateway API key (or a cloud identity token).

The `model` a client requests must exist as a model name on the Models page (a deployment in the API).

The API Keys page has a **Connect** button on every key (and shows the same thing right
after creating one, with the real key filled in): ready-to-paste configuration for Claude
Code, OpenCode, pi and cURL, generated from the models that key may call. Every signed-in
user also has a **Models** page listing each model the gateway serves with its list price
and current health, so nobody has to guess a model id. What follows is
the same material by hand.

## Claude Code

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=llmr_your_key
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1   # /model lists the key's models
export ANTHROPIC_MODEL=my-model        # a model name from the Models page
claude
```

With discovery on, Claude Code asks the gateway's `/v1/models` at startup and adds what
the key may use to its `/model` picker, labelled "From gateway" — no guessing which names
exist. Claude Code keeps only ids containing `claude` or `anthropic` (case-insensitive), so
name Claude models accordingly (`claude-vertex`, `claude-sonnet-5`); other models are still
callable by typing their name, or map them onto the built-in slots with
`ANTHROPIC_DEFAULT_SONNET_MODEL` / `ANTHROPIC_DEFAULT_OPUS_MODEL` /
`ANTHROPIC_DEFAULT_HAIKU_MODEL`.

Streaming, tool use, and token accounting all work — every request shows up in the
gateway dashboard with cost attribution.

### Claude Code on a Claude subscription

Claude Code sends *its own* subscription OAuth token to whatever `ANTHROPIC_BASE_URL`
points at, so the gateway can observe that traffic without holding any Anthropic
credential: it forwards the token untouched and records the metadata. Billing stays on
the subscription instead of API credits.

1. **Provider** — type `openai_compatible`, base URL `https://api.anthropic.com`, auth
   mode **Forward the caller's token** (`oauth_passthrough`). No key is stored. (Starting
   the gateway with `LLMR_SEED_CLAUDE_SUBSCRIPTION=1` — the compose file's default —
   creates this provider and the models below for you.)
2. **Models** — `api_flavor: anthropic`, one per model name Claude Code asks for
   (`claude-sonnet-5`, plus the small model it uses for background work, e.g.
   `claude-haiku-4-5`), each with the same name upstream. Model discovery does not work
   on a pass-through provider — type the names in. These models work only for a caller
   that sends its own subscription token: the console Playground, workspace agents and
   `/v1/models` for a key sent in `Authorization` leave them out rather than fail.
3. **Point Claude Code at the gateway.** The gateway key travels in `X-Llmr-Key` so that
   `Authorization` stays free for the subscription token:

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_CUSTOM_HEADERS="X-Llmr-Key: llmr_your_key"
claude
```

Do **not** set `ANTHROPIC_AUTH_TOKEN` in this mode — it replaces the subscription token
with a gateway key that Anthropic will reject.

Two caveats worth knowing before debugging this for an afternoon:

- **Spend reads $0.** Subscription traffic has no per-token price, so it is recorded
  unpriced and budgets/spend limits do not constrain it. Watch tokens and the
  prompt-cache hit rate on the dashboard instead.
- **A 429 is ambiguous.** Anthropic enforces subscription quota per account, invisibly
  to the gateway, and rejects requests whose system prompt does not look like Claude
  Code's with the same status. An upstream 429 carrying an empty message usually means
  the request shape, not exhausted quota.

This works because Claude Code presents its own credential and its own identity — the
gateway only forwards bytes. Pointing a *different* client at a subscription provider
does not work and should not be made to work: it would mean forging first-party client
identity to spend quota the subscription does not cover. Other clients use API-key
providers, which is what the rest of this page describes.

Use this with your own subscription, for your own Claude Code. A subscription is a
per-person credential; the gateway is not a way to share one subscription across a team.
Check Anthropic's terms for what your plan allows.

## pi

`~/.pi/agent/models.json` — any provider defined here is merged into pi's catalog:

```json
{
  "providers": {
    "llmr": {
      "baseUrl": "http://localhost:8080/v1",
      "api": "openai-completions",
      "apiKey": "$LLMR_API_KEY",
      "models": [{ "id": "my-model" }]
    }
  }
}
```

pi reads this file, not `/v1/models`, so list every model the key may call (the Connect
button on the key generates exactly this). The file reloads whenever `/model` is opened.
Then `pi --provider llmr --model my-model`. `apiKey` also accepts `!command` to shell out
for the key. For full-fidelity Anthropic features (thinking, cache control) use
`"api": "anthropic-messages"` with `"baseUrl": "http://localhost:8080"` — that routes to
`/v1/messages`. pi's `compat` flags (`supportsEagerToolInputStreaming`,
`forceAdaptiveThinking`, `allowEmptySignature`) exist for proxies; reach for them if a
model rejects a field.

### pi in a workspace

A [workspace](configuration.md#workspaces) has pi preconfigured: the gateway writes
`~/.pi/agent/models.json` into the container on every start and again whenever the model
list changes, pointing at itself with a key minted for that workspace and listing every
model the gateway serves (pass-through models excepted — an agent in a container has no
subscription token to forward). Open the
integrated terminal and run `pi`; its spend is metered against the workspace's key.

Because pi keeps sessions per working directory under `$HOME`, and `$HOME` is on the
workspace volume, the conversation survives a container restart.

## opencode

OpenCode's `/models` picker shows only what the config declares — it does not query
`/v1/models` — so list every model the key may call (again, the Connect button generates
this). `opencode.json` in your project (or `~/.config/opencode/opencode.json`):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "llm-router": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "llm-router",
      "options": {
        "baseURL": "http://localhost:8080/v1",
        "apiKey": "llmr_your_key"
      },
      "models": { "my-model": { "name": "my-model" } }
    }
  },
  "model": "llm-router/my-model"
}
```

## Cursor

Settings → Models → **OpenAI API Key**: paste your `llmr_…` key, enable
**Override OpenAI Base URL** and set `http://localhost:8080/v1` (or your deployed
gateway URL). Cursor lists models from the gateway's `/v1/models` — only the model
aliases your key is allowed to use appear.

## RooCode / Cline (VS Code)

Provider: **OpenAI Compatible** — Base URL `http://localhost:8080/v1`, API key
`llmr_…`, model id = a deployment alias. (RooCode's **Anthropic** provider also
works: set its base URL to `http://localhost:8080` and use the gateway key.)

## OpenRouter as an upstream

OpenRouter can sit behind the gateway like any OpenAI-compatible provider:
create a provider with base URL `https://openrouter.ai/api/v1`, auth `bearer`,
your OpenRouter key — then deploy any OpenRouter model id under an alias.

## Observability (Langfuse)

Settings → Observability: point the gateway at a Langfuse project (host, public +
secret key) and enable. Every request becomes a trace + generation with model,
token usage, cost, latency, status, and the key/user/team ids as metadata — filter
by `userId`, tags, or metadata in Langfuse. "Capture message content" additionally
sends prompts/completions (off by default; content never touches the gateway's
own database either way). The exporter runs on the control plane, so edge-node
traffic is exported too.
