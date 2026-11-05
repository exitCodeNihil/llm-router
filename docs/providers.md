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
| GKE, EKS, AKS, any other Kubernetes | The pod's service account token, [below](#on-gke-eks-and-aks-beta). No cloud IAM setup |
| Keycloak, Zitadel, Okta or another OIDC provider | A small job that fetches a client-credentials token and writes it to the file well before each expiry, with an audience mapper for `https://api.anthropic.com` |
| GitHub Actions, Cloud Run, Lambda, ECS, VMs | The platform's token ([Anthropic's guides](https://platform.claude.com/docs/en/manage-claude/workload-identity-federation#identity-providers)), written to the file by a sidecar or job. llm-router reads a file or variable; it doesn't call Google's metadata server, AWS STS or Azure IMDS itself |

- Tokens are cached and refreshed two minutes before expiry. If that fails the current one keeps serving
  until 30 seconds before expiry, then requests on that deployment fail over.
- **Identity tokens with a `jti` claim are single use**, and Kubernetes and Keycloak tokens have one. A
  restarted process can't reuse the token its predecessor exchanged, so it fails until the file rotates.
  To avoid that window, turn off `check_jti` on the issuer, which removes replay protection for every
  rule on it.
- A failed exchange answers `Authentication failed` whatever the cause. The reason is on the Console's
  **Authentication events** tab.

### On GKE, EKS and AKS (beta)

**Beta.** Tested against real Anthropic on a local Kubernetes 1.32 cluster with inline keys, not yet on real
GKE, EKS or AKS clusters, where Anthropic fetches the keys through discovery. Please report what you find.

Every cluster signs a token for each pod, and Anthropic can trust the cluster as an issuer. The setup is
the same on all three; only the issuer URL differs. Nothing is needed on the cloud's IAM side.

| Cluster | Get the issuer URL | Looks like | Anthropic's guide |
|---|---|---|---|
| GKE | `kubectl get --raw /.well-known/openid-configuration \| jq -r .issuer` | `https://container.googleapis.com/v1/projects/P/locations/L/clusters/C` | [Kubernetes](https://platform.claude.com/docs/en/manage-claude/wif-providers/kubernetes), [Google Cloud](https://platform.claude.com/docs/en/manage-claude/wif-providers/gcp) |
| EKS | `aws eks describe-cluster --name C --query cluster.identity.oidc.issuer --output text` | `https://oidc.eks.us-west-2.amazonaws.com/id/…` | [EKS projected tokens](https://platform.claude.com/docs/en/manage-claude/wif-providers/aws#use-eks-projected-service-account-tokens) |
| AKS | `az aks show -g RG -n C --query oidcIssuerProfile.issuerUrl -o tsv` | `https://eastus.oic.prod-aks.azure.com/<tenant>/<id>/` | [Kubernetes](https://platform.claude.com/docs/en/manage-claude/wif-providers/kubernetes), [AKS OIDC issuer](https://learn.microsoft.com/en-us/azure/aks/use-oidc-issuer) |

On AKS, new clusters on 1.34 or later have the issuer on. Otherwise run
`az aks update -g RG -n C --enable-oidc-issuer` first, which restarts the API server briefly.

1. **Create the issuer, service account and rule** with **Connect workload** in the Console, on the
   provider's tile:
   - Issuer URL: the value above, exact, including AKS's trailing slash. JWKS source: discovery. If Anthropic
     can't fetch the keys, use inline keys from `kubectl get --raw /openid/v1/jwks`.
   - Rule subject: `system:serviceaccount:<namespace>:<release>-llm-router`. With gateways too, end it with
     `*`, which also matches `<release>-llm-router-gateway`.
   - Rule audience: `https://api.anthropic.com`. Set it explicitly, so the rule can't match the default token
     every pod in the cluster already has.
   - Token lifetime: the suggested 10 minutes works. 1 hour exchanges about half as often.
2. **Install the chart** with the token mounted:

   ```bash
   helm upgrade --install llm-router ./deploy/helm/llm-router --namespace llm-router --create-namespace \
     --set existingSecret=llm-router --set anthropicIdentity.enabled=true
   ```

   This runs llm-router as its own service account, `<release>-llm-router`, and sets
   `ANTHROPIC_IDENTITY_TOKEN_FILE`. To add gateways, create a group on the Gateways page, then add
   `--set gateway.enabled=true,gateway.joinToken=llmrg_…`.
3. **Add the provider**: type *Anthropic*, auth *Workload identity*, with the rule, service account and
   organization IDs. Leave *Identity token file* empty. Picking a model from the list proves the exchange
   works; the Console's Authentication events tab shows why if it doesn't.

Good to know:

- **A rule belongs to one issuer, and each cluster is its own issuer.** Gateways in a second cluster need
  their own rule and provider. Give each provider's deployments a region label so each cluster prefers its
  own.
- **Restarts.** A rolling update is fine: a new pod gets a new token. A container that crashes and restarts in
  the same pod gets the same token back, which is already used, so its Claude requests fail until the file
  rotates, at most about 9 minutes with `expirationSeconds: 600`. The pod still counts as ready and keeps
  receiving traffic. To recover at once, `kubectl delete pod` it, or turn off `check_jti` as above.
- **Two rules in one pod** can't share a token file, because the second exchange is refused as a replay.
  Providers with identical federation settings share a token, so they're fine. For different rules, mount a
  second file through `extraVolumes` and `gateway.extraVolumes` with a different `expirationSeconds` (660, say) or audience, and set
  that provider's *Identity token file*. A second file with identical settings holds the same token.
- **Keep `expirationSeconds` between 600 and 3600**: Kubernetes' minimum, and the most an issuer accepts by
  default.
- **Don't reuse the cloud's own token.** `AWS_WEB_IDENTITY_TOKEN_FILE` and `AZURE_FEDERATED_TOKEN_FILE` carry
  those clouds' audiences, and the rule rejects them.
