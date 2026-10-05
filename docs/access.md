# Keys, teams and access

## Keys

**API Keys → New key**. **Spends against** sets whose budget and limits the key counts toward:
*Just me*, *A team I'm in* (counts against the team and you), or *Another user* (admins only). The
full key is shown once; only a hash is stored. Send it as `Authorization: Bearer llmr_…`,
`x-api-key`, or `X-Llmr-Key` (the form the [subscription pass-through](clients.md) needs).

## Who may call which model

**Allowed models** can be set on a team, a user and a key. Empty means everything.

- A person's default is the union of their teams' lists; a team with no list allows everything.
- A user's own list replaces that default.
- A key can only narrow it further.

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
with `budget_exceeded` or `rate_limit_exceeded`. Members can't change their own limits. Rate limits
are counted per gateway node, and tokens after the response, so a burst can overshoot.

## Sign-in and SSO

Create the first admin on the setup screen, or sign in with `LLMR_ADMIN_TOKEN`. For SSO, fill in
**Settings → OIDC provider**: Issuer URL, Client ID, Client secret and Redirect URL
(`https://<gateway>/auth/callback`). Any OIDC provider works (Entra ID, Google, Okta, Zitadel,
Keycloak). **Auto-create users** provisions people on their first sign-in.

## Cloud identity tokens

Callers on Azure or GCP can send their platform token instead of a key. The gateway verifies it
locally against the issuer's published keys. Add one under **Settings → Add issuer**:

| | Type | Issuer URL | Audience |
|---|---|---|---|
| Azure Entra ID | `entra` | `https://login.microsoftonline.com/<tenant>/v2.0` | `api://llm-router` |
| Google Cloud | `gcp` | `https://accounts.google.com` | any, e.g. `https://router.example.com` |
| Other OIDC | `oidc` | your issuer | your `aud` |

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
