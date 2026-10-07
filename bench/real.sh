#!/usr/bin/env bash
# Compatibility checks against real providers, no load: two free OpenRouter models, a local LM Studio
# model and a Claude subscription, each direct and through every gateway. Pass/fail plus indicative
# latency; real providers vary too much between runs for a latency comparison. Run it on its own
# after `./run.sh all` (./run.sh real).
#
# Needs OPENROUTER_API_KEY in .env (OpenRouter allows 50 free requests a day; this sends 40), LM
# Studio serving on :1234 with a model loaded (LMS_MODEL picks one), and `claude` logged in to a
# subscription (3 short requests per target). OR_MODELS="a b" picks the OpenRouter models.
set -euo pipefail
cd "$(dirname "$0")"
source ./run.sh
set -a && source .env && set +a
: "${OPENROUTER_API_KEY:?put OPENROUTER_API_KEY in bench/.env}"

lms=host.containers.internal:1234
read -ra models <<< "${OR_MODELS:-$(curl -fsS https://openrouter.ai/api/v1/models |
	jq -r '[.data[] | select((.id | endswith(":free")) and (.supported_parameters | index("tools")))][:2][].id' | xargs)}"
LMS_MODEL=${LMS_MODEL:-$(curl -fsS http://127.0.0.1:1234/v1/models | jq -r '.data[0].id')}
log "real providers: ${models[*]} and LM Studio $LMS_MODEL"
printf '%s\n' "${models[@]}" "$LMS_MODEL" > "$OUT/real-models.txt"

# Each gateway serves the same three names: or-a, or-b (OpenRouter) and lms (LM Studio).
cat > "$OUT/litellm-real.yaml" <<EOF
model_list:
  - { model_name: or-a, litellm_params: { model: "openrouter/${models[0]}", api_key: os.environ/OPENROUTER_API_KEY } }
  - { model_name: or-b, litellm_params: { model: "openrouter/${models[1]}", api_key: os.environ/OPENROUTER_API_KEY } }
  - { model_name: lms, litellm_params: { model: "openai/$LMS_MODEL", api_base: "http://$lms/v1", api_key: x } }
  - { model_name: "claude-*", litellm_params: { model: "anthropic/claude-*" } }
general_settings:
  master_key: os.environ/LITELLM_MASTER_KEY
  database_url: os.environ/DATABASE_URL
  # The documented way to use a Claude subscription through LiteLLM.
  forward_client_headers_to_llm_api: true
litellm_settings: { use_chat_completions_url_for_anthropic_messages: true }
EOF
cat > "$OUT/otari-real.yml" <<EOF
database_url: "postgresql://bench:bench@pg:5432/bench"
master_key: \${OTARI_MASTER_KEY}
require_pricing: false
model_discovery: false
providers:
  openrouter: { api_key: \${OPENROUTER_API_KEY} }
  lms: { provider_type: openai, api_base: "http://$lms/v1", api_key: x, models: ["$LMS_MODEL"] }
aliases:
  or-a: "openrouter:${models[0]}"
  or-b: "openrouter:${models[1]}"
  lms: "lms:$LMS_MODEL"
EOF
cat > "$OUT/litellm-rust-real.yaml" <<EOF
model_list:
  - { model_name: or-a, litellm_params: { model: "openai_like/${models[0]}", api_base: "https://openrouter.ai/api/v1", api_key: os.environ/OPENROUTER_API_KEY } }
  - { model_name: or-b, litellm_params: { model: "openai_like/${models[1]}", api_base: "https://openrouter.ai/api/v1", api_key: os.environ/OPENROUTER_API_KEY } }
  - { model_name: lms, litellm_params: { model: "openai_like/$LMS_MODEL", api_base: "http://$lms/v1", api_key: x } }
general_settings: { master_key: sk-bench-master }
EOF
export LITELLM_CONFIG=$OUT/litellm-real.yaml OTARI_CONFIG=$OUT/otari-real.yml LRUST_CONFIG=$OUT/litellm-rust-real.yaml SEED_SUB=1
# Startup probes (wait_key) use the local model, so they don't spend the OpenRouter allowance.
export PROBE_MODEL=lms

setup_router_real() {
	local r=http://127.0.0.1:18080 orp lmp uid
	api() { curl -fsS -H 'Authorization: Bearer bench-admin-token' -H 'Content-Type: application/json' "$@"; }
	orp=$(api -d "{\"name\":\"openrouter\",\"type\":\"openrouter\",\"auth_mode\":\"bearer\",\"api_key\":\"$OPENROUTER_API_KEY\"}" $r/api/providers | jq -r .id)
	lmp=$(api -d "{\"name\":\"lms\",\"type\":\"openai_compatible\",\"base_url\":\"http://$lms/v1\",\"auth_mode\":\"none\"}" $r/api/providers | jq -r .id)
	for m in "or-a ${models[0]} $orp" "or-b ${models[1]} $orp" "lms $LMS_MODEL $lmp"; do
		read -r name up pid <<< "$m"
		api -d "{\"provider_id\":\"$pid\",\"model_name\":\"$name\",\"upstream_name\":\"$up\",\"api_flavor\":\"openai\"}" $r/api/deployments >/dev/null
	done
	uid=$(api -d '{"email":"bench@example.com"}' $r/api/users | jq -r .id)
	api -d "{\"user_id\":\"$uid\",\"name\":\"bench\"}" $r/api/keys | jq -r .key > "$OUT/.key-router"
}

# target_url GW: base URL for OpenAI-format calls from the load generator container.
real_url() {
	case $1 in
	direct-or) echo https://openrouter.ai/api/v1/chat/completions ;;
	direct-lms) echo "http://$lms/v1/chat/completions" ;;
	*) url "$1" openai oai ;;
	esac
}

# check NAME GW MODEL: non-stream, two streams and a tool call; the request results in $OUT.
check() {
	local name=$1 gw=$2 m=$3 key auth=() u
	case $gw in
	direct-or) key=$OPENROUTER_API_KEY ;;
	direct-lms) key=x ;;
	*) key=$(cat "$OUT/.key-$gw") ;;
	esac
	u=$(real_url "$gw")
	dc run --rm --no-deps -T loadgen -url "$u" -model "$m" -H "Authorization: Bearer $key" -rate 0.25 -duration 4s -warmup 0s \
		-prompt 64 -max-tokens 64 -timeout 120s -tag gw="$gw" -out "/results/$RUN/$name-nonstream.json" 2>&1 | tail -1 | tee -a "$OUT/run.log" >&2
	dc run --rm --no-deps -T loadgen -url "$u" -model "$m" -H "Authorization: Bearer $key" -rate 0.25 -duration 8s -warmup 0s \
		-stream -prompt 64 -max-tokens 64 -timeout 120s -tag gw="$gw" -out "/results/$RUN/$name-stream.json" 2>&1 | tail -1 | tee -a "$OUT/run.log" >&2
	# One tool call that the prompt asks for, from the host through the published ports.
	case $gw in
	direct-or) u=https://openrouter.ai/api/v1/chat/completions ;;
	direct-lms) u=http://127.0.0.1:1234/v1/chat/completions ;;
	*) u=http://127.0.0.1:$(port "$gw")$([[ $gw == otari ]] && echo /api)/v1/chat/completions ;;
	esac
	curl -sS -m 120 -H "Authorization: Bearer $key" -H 'Content-Type: application/json' -d "{\"model\":\"$m\",\"max_tokens\":256,
		\"messages\":[{\"role\":\"user\",\"content\":\"What is the weather in Paris? Use the get_weather tool.\"}],
		\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"description\":\"Current weather for a city\",
		\"parameters\":{\"type\":\"object\",\"properties\":{\"city\":{\"type\":\"string\"}},\"required\":[\"city\"]}}}]}" \
		"$u" > "$OUT/$name-tool.json" || true
	log "$name tool call: $(jq -c '.choices[0].message.tool_calls[0].function // .error // "none"' "$OUT/$name-tool.json" 2>/dev/null || echo invalid)"
	sleep 4
}

# claude_p NAME [env...]: three one-word Claude Code requests on the subscription.
claude_p() {
	local name=$1 i t0 ok=0 ms=()
	shift
	for i in 1 2 3; do
		t0=$(now_ms)
		if env -u CLAUDECODE "$@" claude -p --model haiku "Reply with the single word OK." > "$OUT/$name-$i.txt" 2>&1 && grep -qi ok "$OUT/$name-$i.txt"; then ((ok++)) || true; fi
		ms+=($(($(now_ms) - t0)))
	done
	jq -n --arg name "$name" --argjson ok "$ok" --argjson ms "[$(IFS=,; echo "${ms[*]}")]" '{name: $name, ok: $ok, of: 3, ms: $ms}' > "$OUT/$name.json"
	log "$name: $ok/3 ok, ${ms[*]} ms"
}

down
dc up -d mock >/dev/null
for m in or-a or-b; do check "real-direct-$m" direct-or "${models[$([[ $m == or-a ]] && echo 0 || echo 1)]}"; done
check real-direct-lms direct-lms "$LMS_MODEL"
claude_p real-claude-direct

for gw in "${ALL[@]}"; do
	down
	log "starting $gw (real providers)"
	[[ $gw == lrust ]] && setup_lrust
	start "$gw" 1
	case $gw in router) setup_router_real ;; litellm) setup_litellm ;; otari) setup_otari ;; esac
	since=$(perl -MTime::HiRes=time -e 'printf "%d\n", time*1000')
	for m in or-a or-b lms; do check "real-$gw-$m" "$gw" "$m"; done
	case $gw in
	router) claude_p real-claude-router ANTHROPIC_BASE_URL=http://127.0.0.1:18080 "ANTHROPIC_CUSTOM_HEADERS=X-Llmr-Key: $(cat "$OUT/.key-router")" ;;
	litellm) claude_p real-claude-litellm ANTHROPIC_BASE_URL=http://127.0.0.1:14000 "ANTHROPIC_CUSTOM_HEADERS=x-litellm-api-key: Bearer $(cat "$OUT/.key-litellm")" ;;
	esac
	nodb "$gw" || { sleep 65; usage "$gw" "$since" > "$OUT/real-$gw-usage.txt"; log "real $gw usage rows: $(cat "$OUT/real-$gw-usage.txt")"; }
done
down
log "real providers done"
