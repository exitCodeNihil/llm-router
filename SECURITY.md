# Security policy

llm-router handles API keys, provider credentials and (optionally) container access, so
please report vulnerabilities privately.

**Report** through [GitHub private vulnerability reporting](../../security/advisories/new)
(Security tab, "Report a vulnerability"). Please do not open a public issue for anything
exploitable. Include the version, what an attacker can do, and steps to reproduce.

I will acknowledge a report within a few days and aim to ship a fix, with credit if you want
it, before details are made public.

**Supported versions:** the latest release.

**In scope:** the gateway, management API, console, edge protocol, auth (keys, sessions,
SSO, cloud tokens), and the workspace isolation described in
[docs/workspaces.md](docs/workspaces.md).

**Out of scope:** issues that need an attacker who already holds the admin token or the
`LLMR_ENCRYPTION_KEY`, and the workspace container runtime socket being powerful (it is, and
that is documented).

## Data at rest

A gateway writes nothing to disk unless `LLMR_STATE_DIR` is set. With it, the directory holds these
files, readable only by the gateway's user:

- `config.bin`: the group's config as the control plane sent it. That includes the group's provider
  credentials, the SHA-256 hashes of its API keys and of its users' email addresses, limits and
  prices.
- `confirmed.bin`: when that config was last confirmed, and the spend in USD of each budgeted key,
  user and team.
- `refused.bin`, only while the control plane refuses the gateway: the time that began.
- `spool/`: usage events the control plane has not acknowledged yet, deleted once it has.

The three `.bin` files are encrypted with AES-256-GCM under a key derived from the group's join token (HKDF-SHA256
with a random salt per file). Anyone who holds both the disk and the join token can read them, so
protect the volume as you protect the token. Replacing the join token does not re-encrypt files
written under the old one; a gateway deletes them when it starts with the new token, and deletes
the config and spend when the control plane has refused it for 15 minutes.

The spool is not encrypted. Its events hold request and trace ids, the ids of the key, user, team,
deployment and provider, model names, token counts, costs, latency and status. They never hold
prompts, responses, API keys or provider credentials: captured content stays in memory and is left
out of what is written.
