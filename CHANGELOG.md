# Changelog

## v0.1.0 (2026-10-05)

Initial release.

- OpenAI-compatible gateway (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`,
  `/v1/models`) with streaming passthrough and exact token accounting
- Providers: Azure OpenAI / AI Foundry (Entra ID or api-key outbound auth, native price
  catalog), Google Vertex AI (Application Default Credentials, a service-account JSON key,
  or an express-mode API key; Gemini and every Model Garden MaaS model through the
  OpenAI-compatible endpoint, Claude through rawPredict; publisher-catalogue discovery and
  `gcp/` catalog prices), OpenRouter (discovery carries its published prices into the
  model's custom pricing) and any OpenAI-compatible backend (vLLM, LM Studio, TGI, Ollama)
  with custom per-model pricing
- Pass-through auth mode (`oauth_passthrough`): forwards the caller's own upstream
  credential and `anthropic-beta` header instead of storing one, so Claude Code can run
  on a Claude subscription through the gateway (clients authenticate with `X-Llmr-Key`);
  that traffic is always recorded unpriced
- Virtual API keys, users, teams; budgets (daily/monthly/total) and rpm/tpm rate limits
  per key/user/team; model policy on teams, users and keys (a person's default is the union of
  their teams' policies; keys only narrow), per-member budget shares within a team,
  member-minted team keys, and a Models page for every signed-in user
- Inbound Azure Entra ID / GCP identity-token authentication with claim mapping and
  optional auto-provisioning
- Web console: dashboards, key management, teams, providers/deployments, pricing,
  OIDC SSO, RBAC (admin / team admin / member)
- Hardening from the pre-release review: `LLMR_ENCRYPTION_KEY` is required and
  placeholder values are refused (it signs console sessions); the workspace IDE is served
  from its own origin (`LLMR_IDE_LISTEN` / `LLMR_IDE_ORIGIN`) and the proxy strips the
  caller's credentials before the container; console mutations are checked against the
  browser's `Origin`; keys of disabled users stop working; deleting a user removes their
  workspace containers and volumes; deleting a team asks before revoking its keys; the
  last enabled admin cannot be disabled or demoted and first-run setup only opens on an
  empty database; members cannot set budgets or rate limits on their own keys; database
  errors are no longer echoed to clients; streamed Claude usage counts cache writes
- Edge gateway mode: config long-poll sync, sealed disk cache for cold starts and
  control-plane outages, batched usage shipping
- Workspaces: per-user containers with VS Code (code-server), a terminal and a pi-powered
  chat; admin-managed templates (base, python, node, go, java, infra) with in-console
  Dockerfile editing and image builds; per-user files injected into every workspace;
  `$HOME` on the volume so toolchain caches persist; Docker/podman and Kubernetes runtimes
- Deploy: single static binary, Docker image, docker-compose, Helm chart (both modes)
