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
