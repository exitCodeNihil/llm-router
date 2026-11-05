# Providers and models

A **provider** is a backend the gateway sends requests to. A **model** is the name clients send,
mapped to an upstream name on a provider.

## Add a provider

**Providers → Add provider**, then fill in:

| Backend | Type | Base URL | Auth mode |
|---|---|---|---|
| Claude on Anthropic | Anthropic | prefilled | [Workload identity, API key or the caller's login](#anthropic) |
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

## Anthropic

**Providers → Add provider**, type *Anthropic*. The address is prefilled; pick one auth mode:

| Auth mode | Stored | Use it for | Billed to |
|---|---|---|---|
| **Workload identity (no key)** | Nothing secret | Production: nothing long-lived to leak or rotate | The rule's workspace |
| **API key** | `sk-ant-…`, encrypted | Quick starts and local use | The key's workspace |
| **Caller's own login** | Nothing | [Claude Code on a subscription](clients.md#on-your-claudeai-subscription) | The caller's plan; recorded at $0 |
| **Bearer token** | A token, encrypted | A token you already manage | Its workspace |

Then **Models → Add model**: pick from the list, which fills in the catalog price
(`anthropic/claude-opus-5-5`).

### Workload identity

[Workload identity federation](https://platform.claude.com/docs/en/manage-claude/workload-identity-federation)
replaces the `sk-ant-…` key: each gateway presents a short-lived identity token from your identity
provider and gets a short-lived Anthropic token back. llm-router stores no secret and sends Rust gateways
only the IDs below.

1. In the Claude Console, open
   [Settings → Workload identity](https://platform.claude.com/settings/workload-identity-federation).
   **Connect workload** walks you through three things, or create them separately:
   - an **issuer** (`fdis_…`), the identity provider Anthropic should trust. Its URL must match the `iss`
     claim of your tokens exactly;
   - a **service account** (`svac_…`, under Settings → Service accounts), the identity the gateway acts
     as. The *Developer* role is enough;
   - a **rule** (`fdrl_…`) saying which tokens may act as it. Set the subject pattern to your workload's
     `sub` claim, with no wildcard if you can, and leave the scope at `workspace:developer`. Never pick
     `org:admin` for a gateway.

   Identity tokens must carry the audience `https://api.anthropic.com` unless the rule sets another.

   If Anthropic can't reach your identity provider, use **Issuers → Register issuer** with JWKS source
   *Inline keys* and paste the `keys` array from your provider's JWKS. Nothing needs to be exposed to the
   internet, but you must update the keys when they rotate. *Connect workload* asks you to confirm the
   issuer is publicly reachable even for inline keys, so use *Register issuer* in that case.
2. Copy the rule ID, service account ID and your organization ID (Settings → Organization) into the
   provider. Add the workspace ID if the rule covers more than one workspace.
3. Give every process that serves requests, control plane and gateways, its own identity token: the
   provider's *Identity token file*, or `ANTHROPIC_IDENTITY_TOKEN_FILE` or `ANTHROPIC_IDENTITY_TOKEN` in
   its environment.

Where the token comes from depends on your platform:

| Platform | Identity token |
|---|---|
| Kubernetes | Projected service account token, below |
| GitHub Actions, EKS, GKE, AKS | The platform's OIDC token; see [Anthropic's guides](https://platform.claude.com/docs/en/manage-claude/workload-identity-federation#identity-providers) |
| Keycloak, Zitadel, Okta or another OIDC provider | A small job that fetches a client-credentials token and writes it to the file well before each expiry, with an audience mapper for `https://api.anthropic.com` |
| Google Cloud metadata, Azure IMDS, AWS outside EKS | The same: have a sidecar write the file |

On Kubernetes, through the chart's `extraVolumes` and `extraVolumeMounts`:

```yaml
extraVolumes:
  - name: anthropic-token
    projected:
      sources:
        - serviceAccountToken: {path: token, audience: https://api.anthropic.com, expirationSeconds: 600}
extraVolumeMounts:
  - {name: anthropic-token, mountPath: /var/run/secrets/anthropic.com, readOnly: true}
```

- Tokens are cached and refreshed two minutes before expiry. If that fails the current one keeps serving
  until 30 seconds before expiry, then requests on that deployment fail over.
- **Identity tokens with a `jti` claim are single use**, and Kubernetes and Keycloak tokens have one. A
  restarted process can't reuse the token its predecessor exchanged, so it fails until the file rotates:
  with `expirationSeconds: 600` that is at most about 8 minutes. To avoid that window, turn off `check_jti`
  on the issuer, which removes replay protection for every rule on it.
- A failed exchange answers `Authentication failed` whatever the cause. The reason is on the Console's
  **Authentication events** tab.
