#!/usr/bin/env bash
# One command from clone to a running gateway.
#   ./deploy/quickstart.sh           pull the published image and start
#   ./deploy/quickstart.sh --build   build the image from source instead
# Generates deploy/.env once. It is never overwritten: LLMR_ENCRYPTION_KEY protects stored
# provider credentials, and a different key makes them unreadable.
set -euo pipefail
cd "$(dirname "$0")"

ENGINE=${ENGINE:-$(command -v podman >/dev/null && echo podman || echo docker)}
command -v "$ENGINE" >/dev/null || { echo "need podman or docker (or set ENGINE=...)"; exit 1; }
command -v openssl >/dev/null || { echo "need openssl to generate secrets"; exit 1; }

if [ ! -f .env ]; then
  umask 077
  printf 'LLMR_ADMIN_TOKEN=%s\nLLMR_ENCRYPTION_KEY=%s\n' "$(openssl rand -hex 16)" "$(openssl rand -hex 16)" > .env
  echo "wrote deploy/.env  (back it up: it holds the encryption key)"
fi

if [ "${1:-}" = "--build" ]; then up=(up -d --build); else up=(up -d); fi
"$ENGINE" compose "${up[@]}"

URL=${LLMR_URL:-http://localhost:8080}
printf 'waiting for %s ' "$URL"
for _ in $(seq 1 60); do
  curl -fs "$URL/healthz" >/dev/null 2>&1 && { echo "ok"; break; }
  printf '.'; sleep 1
done
curl -fs "$URL/healthz" >/dev/null 2>&1 || { echo; echo "not healthy after 60s: $ENGINE compose logs llmrouter"; exit 1; }

cat <<MSG

  Console      $URL
  Admin token  $(sed -n 's/^LLMR_ADMIN_TOKEN=//p' .env)   (or create an admin on the setup screen)

  Next: Providers -> Add provider, Models -> Add model, API Keys -> New key.
  Deploying for real? See docs/deploy.md
MSG
