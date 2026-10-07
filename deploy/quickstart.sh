#!/usr/bin/env bash
# One command from clone to a running gateway.
#   ./deploy/quickstart.sh                 pull the published image and start
#   ./deploy/quickstart.sh --build         build the image from source instead
#   ./deploy/quickstart.sh --workspaces    also turn on workspaces (see docs/workspaces.md)
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

up=(up -d); ws=0
for arg in "$@"; do
  case $arg in
    --build) up+=(--build) ;;
    --workspaces) ws=1 ;;
    *) echo "usage: $0 [--build] [--workspaces]"; exit 1 ;;
  esac
done

if [ $ws = 1 ]; then
  # The socket as the engine sees it: under `podman machine` that is a path inside the VM.
  if [ "$ENGINE" = podman ]; then
    sock=$("$ENGINE" info --format '{{.Host.RemoteSocket.Path}}'); sock=${sock#unix://}
  else
    sock=/var/run/docker.sock
  fi
  # Both lines are kept in .env, so a later plain `compose up -d` keeps workspaces on.
  grep -q '^LLMR_CONTAINER_SOCKET=' .env || echo "LLMR_CONTAINER_SOCKET=$sock" >> .env
  grep -q '^COMPOSE_FILE=' .env || echo "COMPOSE_FILE=docker-compose.yml:docker-compose.workspaces.yml" >> .env
fi
"$ENGINE" compose "${up[@]}"

URL=${LLMR_URL:-http://localhost:8080}
printf 'waiting for %s ' "$URL"
for _ in $(seq 1 60); do
  curl -fs "$URL/healthz" >/dev/null 2>&1 && { echo "ok"; break; }
  printf '.'; sleep 1
done
curl -fs "$URL/healthz" >/dev/null 2>&1 || { echo; echo "not healthy after 60s: $ENGINE compose logs llmrouter"; exit 1; }

if [ $ws = 1 ]; then
  token=$(sed -n 's/^LLMR_ADMIN_TOKEN=//p' .env)
  # Leave settings alone if workspaces were already configured.
  if curl -fs -H "Authorization: Bearer $token" "$URL/api/settings/workspaces" | grep -q '"enabled":true'; then
    echo "workspaces already enabled"
  else
    curl -fs -X PUT "$URL/api/settings/workspaces" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
      -d '{"enabled":true,"runtime":"docker","socket":"/var/run/docker.sock","gateway_url":"http://llmrouter:8080","memory_mb":2048,"cpus":2,"budget_usd":5,"max_per_user":5}' \
      && echo "workspaces enabled" || echo "could not enable workspaces: set them up under Workspace settings"
  fi
fi

cat <<MSG

  Console      $URL
  Admin token  $(sed -n 's/^LLMR_ADMIN_TOKEN=//p' .env)   (or create an admin on the setup screen)

  Next: Providers -> Add provider, Models -> Add model, API Keys -> New key.
  Deploying for real? See docs/deploy.md
MSG
if [ $ws = 1 ]; then
  echo "  Workspaces: create your admin account on the setup screen (the admin token has no user),"
  echo "  then Workspaces -> build a template -> New workspace."
fi
