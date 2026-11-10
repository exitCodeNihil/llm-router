# Changelog

## [0.3.0](https://github.com/exitCodeNihil/llm-router/compare/v0.2.2...v0.3.0) (2026-11-10)


### ⚠ BREAKING CHANGES

* OTLP and Langfuse v4 destinations, gateway parity, monitoring ([#17](https://github.com/exitCodeNihil/llm-router/issues/17))

### Features

* OTLP and Langfuse v4 destinations, gateway parity, monitoring ([#17](https://github.com/exitCodeNihil/llm-router/issues/17)) ([de7f090](https://github.com/exitCodeNihil/llm-router/commit/de7f0904b5f54a08d2a94d432a8272b463ba7226))

## [0.2.2](https://github.com/exitCodeNihil/llm-router/compare/v0.2.1...v0.2.2) (2026-11-06)


### Features

* add the kubernetes identity source to the chart ([292ebb6](https://github.com/exitCodeNihil/llm-router/commit/292ebb6d0cf0579eaaf8cbf3dc40d8c3b56741d9))
* get Anthropic identity tokens from the Kubernetes API and renew them early ([d4362b2](https://github.com/exitCodeNihil/llm-router/commit/d4362b2bc4b72bfbf01dcb2b8c24d2418a0b2830))


### Bug Fixes

* record usage when a provider can't be reached ([39e9ade](https://github.com/exitCodeNihil/llm-router/commit/39e9ade3baae34a09201a83be7eb5a5989bec69b))

## [0.2.1](https://github.com/exitCodeNihil/llm-router/compare/v0.2.0...v0.2.1) (2026-11-05)


### Features

* add an Anthropic provider type with its auth modes ([2161d3c](https://github.com/exitCodeNihil/llm-router/commit/2161d3c074ade950a38d51a8bca2274c963b02f8))
* add service account, Anthropic identity and gateways to the chart ([bca701b](https://github.com/exitCodeNihil/llm-router/commit/bca701b90be1ea16d71feba848c27338dca59af2))
* authenticate to Anthropic with workload identity federation instead of a key ([a727dcd](https://github.com/exitCodeNihil/llm-router/commit/a727dcd9989e194da7eafd927b0024ef602cffcf))


### Bug Fixes

* forward upstream rate limit headers, 400 on unsupported routes ([a7263e5](https://github.com/exitCodeNihil/llm-router/commit/a7263e5a99b987c335e7ac83160f11909a694814))
* price cache writes, long prompts, US-only and fast mode like Anthropic bills them ([17abd70](https://github.com/exitCodeNihil/llm-router/commit/17abd70d873a020ab5098a287978ff5aba8167cc))
* repair the spend by team query ([82fb493](https://github.com/exitCodeNihil/llm-router/commit/82fb49321854111dcd825dba46ddd905a83e6676))
* stop spending Anthropic identity tokens twice ([f24cc6d](https://github.com/exitCodeNihil/llm-router/commit/f24cc6dd129094c321d6063d9e51023b89bbecdb))

## [0.2.0](https://github.com/exitCodeNihil/llm-router/compare/v0.1.2...v0.2.0) (2026-10-13)


### ⚠ BREAKING CHANGES

* remove edge nodes and the --mode flag

### Features

* add a Rust data-plane gateway that joins gateway groups ([17ab06d](https://github.com/exitCodeNihil/llm-router/commit/17ab06df18d52106fe2cfc4fbafc6635d357c1bf))
* add gateway groups and a sync protocol for data-plane gateways ([3eee415](https://github.com/exitCodeNihil/llm-router/commit/3eee415d6516d4ba5840a77989575d868382aff5))


### Performance Improvements

* encode gateway usage batches off the request threads ([a9f1472](https://github.com/exitCodeNihil/llm-router/commit/a9f14725843f8f7a5e4c8ed6f63fdb0a1848a393))


### Code Refactoring

* remove edge nodes and the --mode flag ([a902c36](https://github.com/exitCodeNihil/llm-router/commit/a902c36d428a7424ca1c9ee4534086ff6bbe2108))

## [0.1.2](https://github.com/exitCodeNihil/llm-router/compare/v0.1.1...v0.1.2) (2026-10-07)


### Features

* run workspaces from a containerised gateway ([#8](https://github.com/exitCodeNihil/llm-router/issues/8)) ([00b40e4](https://github.com/exitCodeNihil/llm-router/commit/00b40e4661b44bb0cfc0d6e40c33a54a2e1a72b2))

## [0.1.1](https://github.com/exitCodeNihil/llm-router/compare/v0.1.0...v0.1.1) (2026-10-06)


### Features

* optional Langfuse stack in the compose file ([#3](https://github.com/exitCodeNihil/llm-router/issues/3)) ([ec16b29](https://github.com/exitCodeNihil/llm-router/commit/ec16b29584909d3ed1a0bf28bfb5df31cdb56794))


### Bug Fixes

* warn that pass-through models can't be used in the Playground ([#2](https://github.com/exitCodeNihil/llm-router/issues/2)) ([7626597](https://github.com/exitCodeNihil/llm-router/commit/7626597ca1cb36be3794a0e8cc123c9c2eb6afb3))

## v0.1.0 (2026-10-05)

Initial release.

- OpenAI-compatible gateway (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`,
  `/v1/models`) with streaming passthrough and exact token accounting
- Providers: Azure OpenAI / AI Foundry (Entra ID or api-key outbound auth, native price
  catalog), Google Vertex AI (Application Default Credentials or a service-account JSON key;
  Gemini and every Model Garden MaaS model through the
  OpenAI-compatible endpoint, Claude through rawPredict; publisher-catalogue discovery and
  `gcp/` catalog prices), OpenRouter (discovery fills in its published prices from the
  catalog) and any OpenAI-compatible backend (vLLM, LM Studio, TGI, Ollama)
  with custom per-model pricing
- Pass-through auth mode (`oauth_passthrough`): forwards the caller's own upstream
  credential and `anthropic-beta` header instead of storing one, so Claude Code can run
  on a Claude subscription through the gateway (clients authenticate with `X-Llmr-Key`);
  that traffic is always recorded unpriced. `LLMR_SEED_CLAUDE_SUBSCRIPTION=1` seeds it with
  one `claude-*` model, so new Claude models need no gateway update
- Wildcard model names: a model name ending in `*` serves every name with that prefix
  (exact names first, then the longest prefix), with the requested name sent upstream when
  the upstream name ends in `*` too
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
