# Native cloud identity tokens

Callers already authenticated to Azure or GCP can use their platform identity token as the
gateway credential — no API key distribution needed. The gateway verifies the JWT locally
against the issuer's published keys (JWKS, cached), so validation adds no network hop after
the first request per issuer.

## Azure Entra ID

1. Create an app registration that represents the gateway (e.g. Application ID URI
   `api://llm-router`), set `accessTokenAcceptedVersion: 2` in its manifest.
2. In the console (Settings → Cloud Token Auth) add an issuer:
   - type: `entra`
   - issuer_url: `https://login.microsoftonline.com/<tenant-id>/v2.0`
   - audience: `api://llm-router`
   - claim_mapping: `{"email_claim": "preferred_username", "match": {"tid": "<tenant-id>"}}`
   - optionally `map_to_team` (team UUID) and `auto_create_user: true`
3. Call the gateway:

```bash
TOKEN=$(az account get-access-token --resource api://llm-router --query accessToken -o tsv)
curl https://router.example.com/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

The caller is resolved to the gateway user whose email matches the token's
`preferred_username`; team budgets and rate limits apply as usual.

## GCP

1. Add an issuer: type `gcp`, issuer_url `https://accounts.google.com`, audience of your
   choice (e.g. `https://router.example.com`).
2. Call:

```bash
TOKEN=$(gcloud auth print-identity-token --audiences=https://router.example.com)
curl https://router.example.com/v1/chat/completions -H "Authorization: Bearer $TOKEN" ...
```

For service accounts, mint an ID token with the same audience
(`gcloud auth print-identity-token --impersonate-service-account=...`).

## Notes

- Users unknown to the gateway are rejected unless the issuer sets `auto_create_user`
  (created as `member`, optionally joined to `map_to_team`).
- Validated tokens are cached (keyed by hash) for up to 5 minutes or the token expiry,
  whichever is sooner.
- Edge gateways verify tokens locally too, but cannot auto-provision users — first-seen
  users must reach the control plane once (or be created in the console).

## Generic OIDC (Zitadel, Keycloak, Okta, …)

Any OIDC identity provider works, for both console SSO and inbound bearer tokens:

1. **Console SSO**: Settings → Single sign-on — issuer URL, client id/secret of a web app
   whose redirect is `https://<gateway>/auth/callback`. If the IdP doesn't put `email` in
   the ID token, the gateway falls back to the UserInfo endpoint automatically.
2. **Machine-to-machine tokens**: add a token issuer of type `oidc` with your issuer URL and
   the expected `aud`. If the tokens carry no `email` claim (service accounts), point
   `email_claim` at any stable identity claim — e.g. Zitadel client-credentials JWTs carry
   `client_id`, so `{"email_claim": "client_id", "auto_create_user": true}` maps each
   service account to its own gateway user with its own budgets and rate limits.

```bash
TOKEN=$(curl -s https://idp.example.com/oauth/v2/token \
  -d "grant_type=client_credentials&client_id=svc&client_secret=…&scope=openid" | jq -r .access_token)
curl https://gateway.example.com/v1/chat/completions -H "Authorization: Bearer $TOKEN" ...
```
