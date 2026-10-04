#!/usr/bin/env bash
# One-shot Zitadel bootstrap for the dev stack (docker-compose.dev.yml):
# creates a project, an OIDC web app for console SSO, a human test user, and a
# machine user for token-exchange testing. Idempotent enough to re-run (already-
# exists errors are harmless). Prints everything you need to paste into the
# gateway console.
set -euo pipefail

Z=http://localhost:8082
PAT_FILE="$(dirname "$0")/zitadel-pat/pat.txt"
GATEWAY_URL=${GATEWAY_URL:-http://localhost:8080}

echo "waiting for zitadel..."
for _ in $(seq 1 90); do
  curl -sf $Z/debug/healthz >/dev/null 2>&1 && break
  sleep 2
done
[ -f "$PAT_FILE" ] || { echo "PAT not found at $PAT_FILE — is the compose stack up?"; exit 1; }
PAT=$(tr -d '\n' < "$PAT_FILE")
auth=(-H "Authorization: Bearer $PAT")

# Zitadel v4 defaults to the login-v2 UI, which is a separate container; fall
# back to the embedded v1 login so SSO works out of the box.
curl -s -X PUT $Z/v2/features/instance "${auth[@]}" -d '{"loginV2":{"required":false}}' >/dev/null

PROJ=$(curl -s -X POST $Z/management/v1/projects "${auth[@]}" -d '{"name":"llm-router"}' \
  | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')
echo "project: ${PROJ:-'(already exists)'}"

if [ -n "$PROJ" ]; then
  APP=$(curl -s -X POST $Z/management/v1/projects/$PROJ/apps/oidc "${auth[@]}" -d "{
    \"name\":\"console-sso\",
    \"redirectUris\":[\"$GATEWAY_URL/auth/callback\"],
    \"responseTypes\":[\"OIDC_RESPONSE_TYPE_CODE\"],
    \"grantTypes\":[\"OIDC_GRANT_TYPE_AUTHORIZATION_CODE\"],
    \"appType\":\"OIDC_APP_TYPE_WEB\",
    \"authMethodType\":\"OIDC_AUTH_METHOD_TYPE_BASIC\",
    \"devMode\":true}")
  CLIENT_ID=$(echo "$APP" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("clientId",""))')
  CLIENT_SECRET=$(echo "$APP" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("clientSecret",""))')
fi

curl -s -X POST $Z/management/v1/users/human/_import "${auth[@]}" -d '{
  "userName":"testdev","profile":{"firstName":"Test","lastName":"Dev"},
  "email":{"email":"testdev@example.com","isEmailVerified":true},
  "password":"Password1!","passwordChangeRequired":false}' >/dev/null

MID=$(curl -s -X POST $Z/management/v1/users/machine "${auth[@]}" \
  -d '{"userName":"svc-caller","name":"svc-caller","accessTokenType":"ACCESS_TOKEN_TYPE_JWT"}' \
  | python3 -c 'import json,sys;print(json.load(sys.stdin).get("userId",""))')
if [ -n "$MID" ]; then
  MSECRET=$(curl -s -X PUT $Z/management/v1/users/$MID/secret "${auth[@]}" -d '{}' \
    | python3 -c 'import json,sys;print(json.load(sys.stdin).get("clientSecret",""))')
fi

cat <<EOF

── credentials ────────────────────────────────────────────────────────────────
Zitadel console    http://localhost:8082/ui/console
                   zitadel-admin@zitadel.localhost / Password1!
Zitadel SSO user   testdev / Password1!   (testdev@example.com)
Zitadel machine    svc-caller / ${MSECRET:-"(already existed — reset its secret in the Zitadel console)"}
Langfuse UI        http://localhost:3001
                   admin@example.com / Password1!
Langfuse API keys  public pk-lf-dev-1234 · secret sk-lf-dev-1234
Gateway Postgres   postgres://llmrouter:llmrouter@localhost:5433/llmrouter

── paste into the gateway console ─────────────────────────────────────────────
Settings → Single sign-on:
  Issuer URL:    $Z
  Client ID:     ${CLIENT_ID:-"(app already existed — find it in the Zitadel console)"}
  Client secret: ${CLIENT_SECRET:-"(see above)"}
  Redirect URL:  $GATEWAY_URL/auth/callback
  → then log in via SSO as: testdev / Password1!

Settings → Cloud token auth → Add issuer:
  Type: Generic OIDC · Issuer URL: $Z · Audience: svc-caller
  Claim mapping: email claim "client_id", auto-create user ON

Settings → Observability:
  Host: http://localhost:3001 · Public key: pk-lf-dev-1234 · Secret key: sk-lf-dev-1234
  Langfuse UI: http://localhost:3001 (admin@example.com / Password1!)

Token-exchange smoke test:
  TOKEN=\$(curl -s $Z/oauth/v2/token -d "grant_type=client_credentials&client_id=svc-caller&client_secret=${MSECRET:-<secret>}&scope=openid" | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
  curl $GATEWAY_URL/v1/chat/completions -H "Authorization: Bearer \$TOKEN" -d '{"model":"<alias>","messages":[{"role":"user","content":"hi"}]}'
───────────────────────────────────────────────────────────────────────────────
EOF
