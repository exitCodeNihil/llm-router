# Providers and models

A **provider** is a backend the gateway sends requests to. A **model** is the name clients send,
mapped to an upstream name on a provider.

## Add a provider

**Providers → Add provider**, then fill in:

| Backend | Type | Base URL | Auth mode |
|---|---|---|---|
| Azure OpenAI | Azure OpenAI / AI Foundry | `https://<resource>.openai.azure.com` | Entra ID (managed identity), or API key |
| Claude on Azure AI Foundry | Azure OpenAI / AI Foundry | `https://<resource>.services.ai.azure.com` | API key |
| Gemini or Claude on Vertex AI | Google Vertex AI | blank | Application Default Credentials, or Service account key (JSON) |
| OpenRouter | OpenRouter | prefilled | API key |
| vLLM, LM Studio, Ollama, OpenAI | OpenAI-compatible | `http://host:8000/v1` (include `/v1`) | Bearer token, or No auth |

Vertex also asks for **Project ID** and **Location** (`global` by default; use a region such as
`us-east5` if your Claude model is only served there). Stored keys are encrypted with
`LLMR_ENCRYPTION_KEY`; Entra ID, Application Default Credentials and No auth store nothing.
Entra ID on Foundry needs the "Azure AI User" role on the resource.

## Add a model

**Models → Add model**, pick the provider, then:

- **Model name**: what clients send as `model`.
- **Upstream model**: the provider's own name. The field lists what the provider serves.
  Examples: an Azure deployment name (`gpt-4o`), `google/gemini-2.5-flash` or
  `claude-sonnet-4-5@20250929` on Vertex, an OpenRouter slug (`anthropic/claude-sonnet-4-5`).
- **Pricing**: *Catalog* (prefilled when you pick from the list) or *Custom* ($ per 1M tokens;
  `0` is a valid price for local models). With no price, cost is recorded as unpriced.
- **Priority**: lower runs first (default 0).

Upstream names starting with `claude` use the Anthropic protocol automatically, which Foundry and
Vertex require. A provider can serve a given upstream model under one model name only.

**Patterns**: a model name ending in `*` matches every name with that prefix. If the upstream
name ends in `*` too, the requested name is sent on, so `claude-*` with upstream `claude-*` serves
every Claude model, including ones released later. Exact names win, then the longest prefix. A
pattern has one price for every name it serves.

## Routing

Give several models the same **Model name** (the same model on Vertex and OpenRouter, say). The
gateway tries them by priority, then cheapest list price. It moves on at a connection error, 429 or
5xx before anything has streamed, and skips a failing backend for 30 s, doubling to 5 min.

## Check it

```bash
curl localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer llmr_…" -H "Content-Type: application/json" \
  -d '{"model":"my-model","messages":[{"role":"user","content":"hi"}]}'
```

The response headers `X-Llmr-Provider` and `X-Llmr-Upstream` show which backend answered.

## Gotchas

- `/v1/chat/completions` returns `404 model_not_found` when no model has that name and
  `404 model_not_allowed` when the key, its user or its team has an allow-list that excludes it
  ([access](access.md#who-may-call-which-model)). `/v1/messages` returns `404 not_found_error` for both.
- Inside a container, `localhost` is the container. Reach a server on your machine with
  `host.containers.internal` (podman) or `host.docker.internal` (Docker Desktop).
- Editing a provider's Base URL or auth mode clears its stored key, so enter it again. Changing
  `LLMR_ENCRYPTION_KEY` makes stored keys unreadable and those providers drop out of routing.
- A direct Anthropic API key can't be added in the console. Use Claude on Vertex or Foundry,
  OpenRouter, or the [subscription pass-through](clients.md#on-your-claudeai-subscription).
