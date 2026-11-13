# Keys, teams and access

## Keys

**API Keys → New key**. **Spends against** sets whose budget and limits the key counts toward:
*Just me*, *A team I'm in* (counts against the team and you), or *Another user* (admins only). The
full key is shown once; only a hash is stored. Send it as `Authorization: Bearer llmr_…`,
`x-api-key`, or `X-Llmr-Key` (the form the [subscription pass-through](clients.md) needs).

The console pages for users, keys and teams (and a team's members) load 50 rows at a time and search
on the server, so they stay fast with 100,000 of each. The same lists take `limit` (1 to 200), `after`
and `q` on `GET /api/users`, `/api/keys`, `/api/teams` and `/api/teams/{id}/members`, and then answer
`{"items": [...], "next": "<cursor>"}`: pass `next` as `after` for the following page, and an empty
`next` ends the list. `q` is a case-insensitive substring match (email and name; key name, prefix and
owner; team name). Without `limit` they answer the full array as before, and `q` or `after` without
`limit` is a 400. Paged keys also carry `user_email` and `team_name`. `POST /api/lookup` turns ids into
names: `{"users": [...], "keys": [...], "teams": [...]}`, at most 100 of each kind.

## Who may call which model

**Allowed models** can be set on a team, a user and a key. Empty means everything.

- A person's default is the union of their teams' lists; a team with no list allows everything.
- A user's own list replaces that default.
- A key can only narrow it further.
- Listing a [pattern](providers.md#add-a-model) such as `claude-*` allows every name it serves,
  but not a model that has its own exact name.

A request must pass every list that applies. `/v1/models` on a key returns what it may call.

## Budgets and rate limits

| Limit on | Set at | Caps |
|---|---|---|
| Key | **API Keys → Edit** | that key |
| Member | **Teams → Members**, budget button | what the member spends through that team's keys |
| Team | **Teams → Edit** | all of the team's keys together |
| User | **Users → Edit** | everything that person spends |

Each takes **Budget (USD)** with a **Period** (daily, monthly or total), **Requests / min** and
**Tokens / min**. A request must fit every limit that applies. The first one to fail returns `429`
with `budget_exceeded` or `rate_limit_exceeded`. Members can't change their own limits. Limits hold
across llm-router and every [gateway](deploy.md#gateways): gateways sync their counts every 2 seconds,
and tokens are counted after the response, so a burst can overshoot a little.

## Sign-in and SSO

Create the first admin on the setup screen, or sign in with `LLMR_ADMIN_TOKEN`. For SSO, fill in
**Settings → OIDC provider**: Issuer URL, Client ID, Client secret and Redirect URL
(`https://<gateway>/auth/callback`). Any OIDC provider works (Entra ID, Google, Okta, Zitadel,
Keycloak). **Auto-create users** provisions people on their first sign-in.

The client secret is stored encrypted with `LLMR_ENCRYPTION_KEY` and never shown again. If that key
changes, the secret can no longer be read: SSO sign-in stops, the error is logged, and the settings
page asks for the secret again.

## Deleting and erasing users

Deleting a user removes the account, its keys, team memberships, chats and workspaces. Its email,
and the names of its keys, are kept as labels, so the Dashboard, Analytics and Requests still say
whose usage it was: "ana@example.com (deleted)". The same goes for deleted keys and teams. Deleted
users are listed under **Users → Deleted users**. The email is free to use for a new account.

Erasing removes the person's personal data as well (**Erase** on a deleted user, or **Also erase
this person's data** when deleting; `POST /api/users/{id}/erase`). It replaces the kept email and
key names with "erased", removes the email from the audit log (as actor, and from the recorded
detail of any change made to that user), takes the user off the workspaces allow-list, and makes
gateways drop the hash of their email. Usage rows keep the user's id, which then names no one.
Erase cannot reach application logs, database backups, or traces already exported to telemetry
destinations: the response lists these, and any workspace volume the runtime could not remove.

## Cloud identity tokens

Callers on Azure or GCP can send their platform token instead of a key. The gateway verifies it
locally against the issuer's published keys. Add one under **Settings → Add issuer**:

| | Type | Issuer URL | Audience |
|---|---|---|---|
| Azure Entra ID | `entra` | `https://login.microsoftonline.com/<tenant>/v2.0` | `api://llm-router` |
| Google Cloud | `gcp` | `https://accounts.google.com` | any, e.g. `https://router.example.com` |
| Other OIDC | `oidc` | your issuer | your `aud` |

The issuer URL must use https (plain http only for `localhost`), and so must the key set it
publishes: the control plane and every gateway fetch keys from it.

For Entra, set `accessTokenAcceptedVersion: 2` in the app registration manifest and **Email claim**
to `preferred_username`. For OIDC machine tokens with no email, point **Email claim** at a stable
claim such as `client_id`. Callers are matched to a gateway user by that claim; unknown users are
rejected unless **Auto-create user** is on (optionally joined to **Map to team**).

```bash
TOKEN=$(az account get-access-token --resource api://llm-router --query accessToken -o tsv)
# or: gcloud auth print-identity-token --audiences=https://router.example.com
curl https://router.example.com/v1/chat/completions -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"my-model","messages":[{"role":"user","content":"hi"}]}'
```

[Gateways](deploy.md#gateways) verify identity tokens too, with the issuer's keys and no call to
the control plane for people they know. A gateway group limited to some teams serves their members,
and creates new users only when **Map to team** is one of its teams. A new person's first request
goes to the control plane once, which checks the token itself before it creates the user, so a
gateway can't create users on its own. The control plane must therefore reach the issuer too (its
`/.well-known/openid-configuration` and `jwks_uri`): while it can't, new people's first requests
fail, the gateway asks again a few seconds later, and nobody is refused for it. To map tokens to
users, gateways get every user they serve and a hash of each email address, not the address.

## Audit log

Every change made through the console or the management API is recorded: who made it (the user,
or the admin token), the action, the record it acted on, the outcome (the HTTP status), the client
address, and which settings changed. Sign-ins, failed sign-ins, SSO sign-ins, sign-outs and the
first-admin setup are recorded too. Refused attempts are recorded as well as successful ones: a
member calling an admin route, a wrong admin token, a request with no credentials.

For settings that are not secret, such as budgets, rate limits, roles, allowed models, names of
teams, keys and deployments, the new values are kept. For everything else only the names of the
fields sent are kept, or nothing. Never recorded: passwords, API keys, client secrets, telemetry
headers and secrets, join tokens, uploaded files, URLs (they can carry credentials), tags, and any
email address other than the acting user's own. A failed sign-in keeps no email at all, since the
address tried is often a typo of someone else's. Reads, chats and the playground are not recorded.

Admins read it on the **Audit log** page, or with `GET /api/audit` (filters: `actor`, `action`,
`target_type`, `target_id`, `status=ok|err`, `since`, `until`; page with `before=<id>` and
`limit`). Each event is also written as an `audit` log line, without the detail or any email, for
shipping to a SIEM.

Rows cannot be updated or deleted: a trigger refuses it. The only exception is retention:
`LLMR_AUDIT_RETENTION_MONTHS` (chart: `retention.auditMonths`) deletes rows older than that many
months. `0`, the default, keeps them for ever. The trigger stops the application and mistakes, not
someone who owns the database: ship the log lines elsewhere if that matters.
