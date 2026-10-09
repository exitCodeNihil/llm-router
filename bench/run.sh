#!/usr/bin/env bash
# Gateway benchmark driver: llm-router vs LiteLLM (Python and Rust) vs Otari against one mock upstream.
# README.md says what each scenario measures and why.
#
#   ./run.sh build             cross-compile the mock and load generator, make the TLS cert
#   ./run.sh build-lrust       build LiteLLM's Rust gateway image from source (no published image)
#   ./run.sh all               every scenario except real (hours: run it overnight)
#   ./run.sh cal               check the CPU and memory sampler before trusting it
#   ./run.sh s3 router otari   one scenario for some gateways (default: GATEWAYS)
#   ./run.sh real              compatibility checks against real providers (needs .env)
#   ./run.sh report            print every run as one table
#   ./run.sh docs              write the summary tables into ../docs/benchmarks.md
#
# RUN names the results directory (default: today), GATEWAYS the gateways to run (default:
# router litellm otari lrust), ROUTER_IMAGE the llm-router image (default: the pinned release),
# REPEATS the headline repeats (3), SOAK_MIN the soak length in minutes (30), QUICK=1 shrinks
# every run to seconds (to test the script). Needs podman (with compose), Go, jq, curl and perl.
set -euo pipefail
cd "$(dirname "$0")"
SELF=$PWD/$(basename "$0")

export RUN=${RUN:-$(date +%F)}
OUT=results/$RUN
REPEATS=${REPEATS:-3}
SOAK_MIN=${SOAK_MIN:-30}
read -ra ALL <<< "${GATEWAYS:-router litellm otari lrust}"
# LiteLLM's Rust gateway, pinned: BerriAI/litellm main on the day of the run.
LRUST_COMMIT=7b439d9138382e6e4b6b0498336c017157dee28e
RATES=(10 25 50 100 200 300 500 750 1000 1500 2000 3000 5000 7500 10000 15000 20000 30000 40000 50000)
export COMPOSE_PROJECT_NAME=llmr-bench GW_CPUS=1 COMPOSE_ANSI=never
# An empty Docker config stops compose from asking the macOS keychain for registry credentials.
export DOCKER_CONFIG=${DOCKER_CONFIG:-$PWD/.docker}
mkdir -p "$OUT" "$DOCKER_CONFIG"
trap '[[ -n ${SAMPLER:-} ]] && kill -- -"$SAMPLER" 2>/dev/null; true' EXIT

dc() { podman compose -f compose.yml "$@" 2> >(grep -v -e '^>>>>' -e 'external compose provider' >&2); }
log() { printf '%s %s\n' "$(date +%T)" "$*" | tee -a "$OUT/run.log" >&2; }
now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }
# vm runs a shell command where the containers' cgroups live: inside the podman VM on macOS.
vm() { if [[ $(uname) == Darwin ]]; then podman machine ssh -- "$@"; else bash -c "$*"; fi; }

build() {
	local arch c
	arch=$(vm uname -m | tr -d '\r')
	[[ $arch == aarch64 || $arch == arm64 ]] && arch=arm64 || arch=amd64
	for c in mock loadgen; do GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -o "bin/$c" "./$c"; done
	mkdir -p bin/tls
	[[ -f bin/tls/cert.pem ]] || openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 365 \
		-subj /CN=mock -addext subjectAltName=DNS:mock -keyout bin/tls/key.pem -out bin/tls/cert.pem 2>/dev/null
	chmod 644 bin/tls/key.pem
	# The gateways trust the system roots plus the mock's certificate.
	cat /etc/ssl/cert.pem /etc/ssl/certs/ca-certificates.crt bin/tls/cert.pem 2>/dev/null > bin/tls/bundle.pem || true
}

# ---- gateways -------------------------------------------------------------------------------

port() { case $1 in router) echo 18080 ;; litellm) echo 14000 ;; otari) echo 18000 ;; lrust) echo 14001 ;; esac }
health() { case $1 in router) echo /healthz ;; litellm) echo /health/liveliness ;; otari) echo /api/v1/health ;; lrust) echo /v1/models ;; esac }
# nodb GW: no database behind it (the direct path, and LiteLLM's Rust gateway, which keeps no state).
nodb() { [[ $1 == direct || $1 == lrust ]]; }

wait_http() {
	local i
	for ((i = 0; i < 600; i++)); do
		curl -fsS -o /dev/null "$1" 2>/dev/null && return 0
		sleep 0.2
	done
	log "gave up waiting for $1"
	return 1
}

# start GW CPUS: (re)start a gateway with this many vCPUs, keeping its database.
start() {
	export GW_CPUS=$2
	local svc=(mock pg "$1")
	nodb "$1" && svc=(mock "$1")
	dc --profile "$1" up -d --quiet-pull "${svc[@]}" >/dev/null
	if [[ $1 == lrust ]]; then wait_key lrust; else wait_http "http://127.0.0.1:$(port "$1")$(health "$1")"; fi
}

# fresh GW: a gateway on 1 vCPU with an empty database, set up and with its keys in $OUT.
fresh() {
	down
	log "starting $1"
	[[ $1 == lrust ]] && setup_lrust
	start "$1" 1
	[[ $1 == lrust ]] || "setup_$1"
	digest "$1"
}

down() { dc --profile router --profile litellm --profile otari --profile lrust down -v --remove-orphans >/dev/null 2>&1 || true; }

digest() {
	local img
	img=$(dc --profile "$1" config --format json | jq -r ".services.$1.image")
	podman image inspect --format '{{.Id}} {{.RepoDigests}}' "$img" > "$OUT/image-$1.txt"
	[[ $1 == lrust ]] && echo "built from BerriAI/litellm@$LRUST_COMMIT" >> "$OUT/image-$1.txt"
	[[ $1 != lrust && $img == localhost/* ]] && echo "built from $(git describe --always --dirty)" >> "$OUT/image-$1.txt"
	true
}

# Each setup writes the keys the scenarios use: .key-GW (plain), .key-GW-limits (huge budget and
# rate limits, to measure the cost of checking them), .key-GW-rpm (600 rpm) and .key-GW-budget ($0.01).
setup_router() {
	local r=http://127.0.0.1:18080 oai anth tls fo bk uid p
	api() { curl -fsS -H 'Authorization: Bearer bench-admin-token' -H 'Content-Type: application/json' "$@"; }
	provider() { api -d "{\"name\":\"$1\",\"type\":\"openai_compatible\",\"base_url\":\"$2\",\"auth_mode\":\"none\"}" $r/api/providers | jq -r .id; }
	deploy() {
		api -d "{\"provider_id\":\"$1\",\"model_name\":\"$2\",\"upstream_name\":\"$3\",\"api_flavor\":\"${4:-openai}\",\"priority\":${5:-0},\"input_per_1m\":1,\"output_per_1m\":2}" \
			$r/api/deployments >/dev/null
	}
	oai=$(provider mock-oai http://mock:9000/v1)
	anth=$(provider mock-anth http://mock:9000)
	tls=$(provider mock-tls https://mock:9443/v1)
	fo=$(provider mock-fo http://mock:9000/v1)
	bk=$(provider mock-bk http://mock:9000/v1)
	for p in fast chat long slow err429 tok10 tok0; do deploy "$oai" $p $p; done
	for p in fast chat long; do deploy "$anth" a-$p $p anthropic; done
	deploy "$tls" tls-chat chat
	# Failover: two deployments under one model name, the failing one first.
	for p in 500:err500 429:err429 flaky:flaky hang:hang; do
		deploy "$fo" "fo${p%%:*}" "${p#*:}.fo" openai 0
		deploy "$bk" "fo${p%%:*}" "fast.fo${p%%:*}" openai 1
	done
	uid=$(api -d '{"email":"bench@example.com"}' $r/api/users | jq -r .id)
	key() { api -d "{\"user_id\":\"$uid\",$1}" $r/api/keys | jq -r .key; }
	key '"name":"bench"' > "$OUT/.key-router"
	key '"name":"limits","budget_usd":1000000,"budget_period":"monthly","rpm_limit":100000000,"tpm_limit":2000000000' > "$OUT/.key-router-limits"
	key '"name":"rpm","rpm_limit":600' > "$OUT/.key-router-rpm"
	key '"name":"budget","budget_usd":0.01,"budget_period":"monthly"' > "$OUT/.key-router-budget"
	wait_key router
}

setup_litellm() {
	key() { curl -fsS -H 'Authorization: Bearer sk-bench-master' -H 'Content-Type: application/json' -d "$1" http://127.0.0.1:14000/key/generate | jq -r .key; }
	key '{}' > "$OUT/.key-litellm"
	key '{"max_budget":1000000,"budget_duration":"30d","rpm_limit":100000000,"tpm_limit":2000000000}' > "$OUT/.key-litellm-limits"
	key '{"rpm_limit":600}' > "$OUT/.key-litellm-rpm"
	key '{"max_budget":0.01}' > "$OUT/.key-litellm-budget"
	wait_key litellm
}

setup_otari() {
	# Otari has no per-key limits: budgets attach to a user, and rate limits are rules that apply to
	# every key, so s9 adds each rule just for the run that tests it (otari_rule).
	local r=http://127.0.0.1:18000/api/v1
	api() { curl -fsS -H 'Authorization: Bearer bench-master-key-0123456789' -H 'Content-Type: application/json' "$@"; }
	user() { api -d "{\"user_id\":\"$1\"${2:+,\"budget_id\":\"$2\"}}" $r/users >/dev/null; }
	budget() { api -d "$1" $r/budgets | jq -r .budget_id; }
	key() { api -d "{\"user_id\":\"$1\",\"key_name\":\"$1\"}" $r/keys | jq -r .key; }
	user bench
	key bench > "$OUT/.key-otari"
	cp "$OUT/.key-otari" "$OUT/.key-otari-rpm"
	user bench-limits "$(budget '{"name":"limits","max_budget":1000000,"budget_duration_sec":2592000}')"
	key bench-limits > "$OUT/.key-otari-limits"
	user bench-budget "$(budget '{"name":"budget","max_budget":0.01}')"
	key bench-budget > "$OUT/.key-otari-budget"
	wait_key otari
}

setup_lrust() { # master key only: the Rust gateway has no virtual keys yet
	echo sk-bench-master > "$OUT/.key-lrust"
}

build_lrust() {
	local src=.lrust-src
	if [[ ! -d $src ]]; then
		git clone -q --filter=blob:none --no-checkout https://github.com/BerriAI/litellm.git "$src"
		git -C "$src" sparse-checkout set litellm-rust >/dev/null
	fi
	git -C "$src" fetch -q --depth 1 origin "$LRUST_COMMIT" && git -C "$src" checkout -q "$LRUST_COMMIT"
	podman build -f "$src/litellm-rust/crates/gateway/Dockerfile" -t "localhost/litellm-gateway:${LRUST_COMMIT:0:8}" "$src/litellm-rust"
}

# otari_rule JSON | otari_rule -NAME: add or delete an Otari rate-limit rule.
otari_rule() {
	local r=http://127.0.0.1:18000/api/v1/rate-limits a='Authorization: Bearer bench-master-key-0123456789'
	if [[ $1 == -* ]]; then curl -fsS -X DELETE -H "$a" "$r/${1#-}" >/dev/null; else curl -fsS -H "$a" -H 'Content-Type: application/json' -d "$1" $r >/dev/null; fi
}

# wait_key GW: config reloads are asynchronous, so poll until the new key serves a request.
wait_key() {
	local i
	for ((i = 0; i < 100; i++)); do
		[[ $(proxied "$1") == 200 ]] && return 0
		sleep 0.3
	done
	log "key for $1 never started working: $(proxied "$1" -v)"
	return 1
}

# proxied GW: one small request through the published port; prints the status code.
proxied() {
	local path=/v1/chat/completions
	[[ $1 == otari ]] && path=/api$path
	curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $(cat "$OUT/.key-$1")" -H 'Content-Type: application/json' \
		-d "{\"model\":\"${PROBE_MODEL:-$(model "$1" oai fast)}\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
		"http://127.0.0.1:$(port "$1")$path" 2>/dev/null || true
}

# model GW UPSTREAM PROFILE: the name a gateway serves a mock profile under. UPSTREAM is oai or
# anth (the mock's OpenAI or Anthropic API), tls (OpenAI API over HTTPS) or fo (a failover pair).
model() {
	case $1:$2 in
	direct:* | *:fo) echo "$3" ;;
	otari:*) echo "mock_$2:$3" ;;
	*:anth) echo "a-$3" ;;
	*:tls) echo "tls-$3" ;;
	*) echo "$3" ;;
	esac
}

# url GW API UPSTREAM
url() {
	local base path=/v1/chat/completions
	[[ $2 == anthropic ]] && path=/v1/messages
	case $1 in
	direct) [[ $3 == tls ]] && base=https://mock:9443 || base=http://mock:9000 ;;
	router) base=http://router:8080 ;;
	litellm) base=http://litellm:4000 ;;
	lrust) base=http://lrust:4000 ;;
	otari) base=http://otari:8000/api ;;
	esac
	echo "$base$path"
}

# ---- measuring ------------------------------------------------------------------------------

# unsupported GW API UPSTREAM ARGS...: a path the gateway doesn't implement, reported as n/a. LiteLLM's
# Rust gateway streams only Anthropic-to-Anthropic, and /v1/messages only reaches Anthropic upstreams.
unsupported() {
	[[ $1 == lrust ]] || return 1
	[[ $2 == anthropic && $3 != anth ]] && return 0
	[[ $2 == openai && " ${*:4} " == *" -stream "* ]]
}

# lg NAME GW API UPSTREAM PROFILE [loadgen flags]: one load run, summary in $OUT/NAME.json.
# KEY picks a key variant (.key-$KEY); NOMOCK=1 skips the mock join (for parallel runs).
lg() {
	local name=$1 gw=$2 api=$3 up=$4 prof=$5 extra=() args=()
	shift 5
	if unsupported "$gw" "$api" "$up" "$@"; then
		echo '{"unsupported": true}' > "$OUT/$name.json"
		log "$name n/a: $gw doesn't support this path"
		return 0
	fi
	# QUICK=1 shrinks every run to a few seconds, to shake out the script rather than measure.
	if [[ -n ${QUICK:-} ]]; then
		while (($#)); do
			case $1 in -duration) args+=(-duration 3s) && shift ;; -warmup) args+=(-warmup 1s) && shift ;; *) args+=("$1") ;; esac
			shift
		done
		set -- "${args[@]}"
	fi
	[[ $gw == direct ]] || extra+=(-H "Authorization: Bearer $(cat "$OUT/.key-${KEY:-$gw}")")
	[[ -n ${NOMOCK:-} ]] || extra+=(-mock http://mock:9000)
	dc run --rm --no-deps -T loadgen -url "$(url "$gw" "$api" "$up")" -api "$api" -model "$(model "$gw" "$up" "$prof")" \
		-out "/results/$RUN/$name.json" -tag gw="$gw" -tag cpus="$GW_CPUS" -tag samples="${SAMPLES:-}" \
		"${extra[@]}" "$@" 2>&1 | tail -1 | sed "s/^/$name /" | tee -a "$OUT/run.log" >&2
}

psql_() { dc exec -T pg psql -U bench -d bench -At -F ' ' -c "$1"; }
dbstat() { psql_ "select xact_commit+xact_rollback, tup_inserted+tup_updated+tup_deleted, (select wal_bytes from pg_stat_wal) from pg_stat_database where datname='bench'"; }

# usage GW SINCE_MS: "rows prompt_tokens completion_tokens" for the successful requests the
# gateway has logged since then (Otari also logs each failed attempt, LiteLLM each failure).
usage() {
	local t="to_timestamp($2/1000.0)"
	case $1 in
	router) psql_ "select count(*), coalesce(sum(prompt_tokens),0), coalesce(sum(completion_tokens),0) from usage_events where ts >= $t and status_code = 200" ;;
	litellm) psql_ "select count(*), coalesce(sum(prompt_tokens),0), coalesce(sum(completion_tokens),0) from \"LiteLLM_SpendLogs\" where \"startTime\" >= $t at time zone 'UTC' and coalesce(status, 'success') = 'success'" ;;
	otari) psql_ "select count(*), coalesce(sum(prompt_tokens),0), coalesce(sum(completion_tokens),0) from usage_logs where timestamp >= $t and status = 'success'" ;;
	esac
}

# drain GW SINCE_MS: wait until the gateway stops writing usage rows (LiteLLM batches them every
# 60 s), then print them.
drain() {
	local a b
	[[ $1 == litellm ]] && sleep 62 || sleep 2
	a=$(usage "$1" "$2")
	while sleep 3 && b=$(usage "$1" "$2") && [[ $a != "$b" ]]; do a=$b; done
	echo "$a"
}

# point NAME GW ...: lg, then the Postgres work and usage rows it caused, in NAME.db.json.
# AFTER=N also saves the mock's stats N seconds after the run (is the upstream call still open?).
point() {
	local name=$1 gw=$2 before after rows since
	nodb "$gw" && {
		lg "$@"
		[[ -z ${AFTER:-} ]] || { sleep "$AFTER"; curl -fsS 127.0.0.1:19000/stats > "$OUT/$name.after.json"; }
		return
	}
	if unsupported "$gw" "$3" "$4" "${@:6}"; then lg "$@"; return; fi
	before=$(dbstat)
	lg "$@"
	if [[ -n ${AFTER:-} ]]; then
		sleep "$AFTER"
		curl -fsS 127.0.0.1:19000/stats > "$OUT/$name.after.json"
	fi
	since=$(jq '.window_start_ms - .warmup_ms - 200' "$OUT/$name.json")
	rows=$(drain "$gw" "$since")
	after=$(dbstat)
	jq -n --arg b "$before" --arg a "$after" --arg r "$rows" '
		[$b, $a, $r] | map(split(" ") | map(tonumber)) as [$b, $a, $r]
		| {xacts: ($a[0]-$b[0]), tuples: ($a[1]-$b[1]), wal_bytes: ($a[2]-$b[2]),
		   rows: $r[0], prompt_tokens: $r[1], completion_tokens: $r[2]}' > "$OUT/$name.db.json"
}

# sample_start NAME / sample_stop: every second, one line per container
# ("ts name cpu_usec throttled_usec nr_throttled mem anon inactive_file sock") and one "ts vm ..."
# line from /proc/stat, read straight from cgroup v2 where the containers run.
sample_start() {
	SAMPLES=samples-$1.txt
	# Job control gives the sampler its own process group, so sample_stop can kill the whole
	# chain (subshell, podman, ssh) and not just the subshell.
	set -m
	vm bash -s > "$OUT/$SAMPLES" 2>/dev/null <<'EOF' &
i=0
while :; do
	# every 5 s: each container's cgroup, wherever podman put it (rootless or rootful)
	if (( i++ % 5 == 0 )); then
		dirs=$(podman ps --format '{{.ID}} {{.Names}}' | while read -r id name; do
			d=$(find /sys/fs/cgroup -maxdepth 8 -type d -name "libpod-$id*" -print -quit 2>/dev/null)
			[[ -n $d ]] && echo "$name $d"
		done)
	fi
	t=$(date +%s.%N)
	while read -r name d; do
		[[ -d $d ]] && awk -v t="$t" -v n="$name" '
			FNR == 1 { f++ }
			f == 1 && $1 == "usage_usec" { u = $2 }  f == 1 && $1 == "throttled_usec" { tu = $2 }
			f == 1 && $1 == "nr_throttled" { nt = $2 } f == 2 { m = $1 }
			f == 3 && $1 == "anon" { a = $2 } f == 3 && $1 == "inactive_file" { fi = $2 } f == 3 && $1 == "sock" { s = $2 }
			END { print t, n, u, tu, nt, m, a, fi, s }' "$d/cpu.stat" "$d/memory.current" "$d/memory.stat"
	done <<< "$dirs"
	echo "$t vm $(head -1 /proc/stat | cut -d' ' -f3-)"
	sleep 1
done
EOF
	SAMPLER=$!
	set +m
}
sample_stop() {
	kill -- -"$SAMPLER" 2>/dev/null || true
	wait "$SAMPLER" 2>/dev/null || true
	SAMPLER= SAMPLES=
}

# ---- scenarios ------------------------------------------------------------------------------

# slo FILE: the run held the bar for "sustainable": <0.1% errors, the rate achieved, per-request
# overhead p99 <= 50 ms, and the load generator itself keeping up.
slo() { jq -e '((.errors | add) // 0) <= 0.001 * .sent and .achieved_rps >= 0.99 * .rate and (.overhead_ms.p99 // 1e9) <= 50 and .lag_ms.p99 <= 20' "$1" >/dev/null; }
# failed FILE: over 1% errors, or the rate not reached, or the gateway adding over 2 s at p99
# (its added time, not the total: a chat stream takes 2.3 s by design).
failed() { jq -e '((.errors | add) // 0) > 0.01 * .sent or .achieved_rps < 0.95 * .rate or (.overhead_ms.p99 // 0) > 2000' "$1" >/dev/null; }

# max GW [CPUS]: the gateway's sustainable rate from its ladder (at least 10).
max() { local m; m=$(cat "$OUT/max-${2:-1}-$1" 2>/dev/null || echo 0); echo $((m > 10 ? m : 10)); }

# slowest CPUS: the lowest sustainable rate among every gateway with a ladder in this run, so a
# later run that adds one gateway to the results keeps the rates the others ran at.
slowest() {
	local f v m=0
	for f in "$OUT/max-$1-"*; do
		[[ -f $f && $f != *-direct ]] || continue
		v=$(<"$f")
		((m == 0 || v < m)) && m=$v
	done
	echo $((m > 10 ? m : 10))
}

# ladder GW: raise the rate until the gateway breaks, then bisect the sustainable maximum to
# within 10%. Writes max-<cpus>-GW.
ladder() {
	local gw=$1 best=0 fail=0 r f
	for r in "${RATES[@]}"; do
		f=s2-ladder-$GW_CPUS-$gw-$r
		lg "$f" "$gw" openai oai fast -rate "$r" -duration 20s -warmup 5s -timeout 10s
		if slo "$OUT/$f.json"; then best=$r; else ((fail)) || fail=$r; fi
		failed "$OUT/$f.json" && break
		sleep 3
	done
	while ((fail > best && fail - best > best / 10)); do
		r=$(((best + fail) / 2))
		((r < 5)) && break
		f=s2-ladder-$GW_CPUS-$gw-$r
		sleep 3
		lg "$f" "$gw" openai oai fast -rate "$r" -duration 20s -warmup 5s -timeout 10s
		if slo "$OUT/$f.json"; then best=$r; else fail=$r; fi
	done
	echo "$best" > "$OUT/max-$GW_CPUS-$gw"
	log "max sustainable $gw at $GW_CPUS vCPU: $best rps"
}

# mockonly: just the mock, for the direct (no gateway) baseline.
mockonly() {
	down
	dc up -d mock >/dev/null
	wait_http http://127.0.0.1:19000/stats
}
target() { if [[ $1 == direct ]]; then mockonly; else fresh "$1"; fi; }

cal() { # the sampler must read ~1.00 core for a busy loop capped at 1 vCPU and ~256 MiB for 256 MiB
	podman rm -f llmr-bench-calbusy-1 llmr-bench-calmem-1 >/dev/null 2>&1 || true
	podman run -d --rm --name llmr-bench-calbusy-1 --cpus 1 docker.io/library/postgres:17 bash -c 'while :; do :; done' >/dev/null
	podman run -d --rm --name llmr-bench-calmem-1 docker.io/mzdotai/otari:0.15.0 python -c 'import time; b = b"x" * (256 << 20); time.sleep(60)' >/dev/null
	sleep 5
	sample_start cal
	sleep 20
	sample_stop
	podman rm -f llmr-bench-calbusy-1 llmr-bench-calmem-1 >/dev/null
	log "sampler check, busy loop: $(go run ./loadgen usage "$OUT/samples-cal.txt" calbusy)"
	log "sampler check, 256 MiB:   $(go run ./loadgen usage "$OUT/samples-cal.txt" calmem)"
}

s1() { # footprint: image size, restart time, idle memory and CPU
	local gw img comp i t0 runs
	for gw in "$@"; do
		fresh "$gw"
		img=$(dc --profile "$gw" config --format json | jq -r ".services.$gw.image")
		comp=null
		if [[ $img != localhost/* ]]; then
			# the download size of the arm64 image: its layers as stored in the registry
			comp=$(vm skopeo inspect --raw "docker://$img" | jq -r '.manifests[] | select(.platform.architecture=="arm64") | .digest' | head -1)
			comp=$(vm skopeo inspect --raw "docker://${img%:*}@$comp" | jq '[.layers[].size] | add')
		fi
		runs=()
		for i in 1 2 3; do
			dc --profile "$gw" stop -t 10 "$gw" >/dev/null
			t0=$(now_ms)
			dc --profile "$gw" start "$gw" >/dev/null
			for ((n = 0; n < 2400; n++)); do [[ $(proxied "$gw") == 200 ]] && break; sleep 0.05; done
			runs+=($(($(now_ms) - t0)))
		done
		sample_start "s1-$gw"
		lg "s1-warm-$gw" "$gw" openai oai fast -rate 50 -duration 10s -warmup 0s
		sleep 65
		jq -n --arg img "$img" --argjson size "$(podman image inspect --format '{{.Size}}' "$img")" --argjson comp "$comp" \
			--argjson cold "[$(IFS=,; echo "${runs[*]}")]" --arg samples "$SAMPLES" \
			--argjson from "$(jq '.window_end_ms + 5000' "$OUT/s1-warm-$gw.json")" \
			'{image: $img, image_bytes: $size, compressed_bytes: $comp, restart_ms: $cold, samples: $samples, idle_from_ms: $from, idle_to_ms: ($from + 55000)}' \
			> "$OUT/s1-$gw.json"
		sample_stop
		log "s1 $gw: $(jq -c . "$OUT/s1-$gw.json")"
	done
	down
}

s2() { # the 1-vCPU ladder for every gateway, and for the direct path (the harness ceiling)
	local gw
	for gw in direct "$@"; do
		target "$gw"
		sample_start "s2-$gw"
		ladder "$gw"
		sample_stop
	done
	down
}

s2c() { # closed-loop concurrency sweep with tiny requests: the shape of an earlier independent study
	local gw c
	for gw in direct "$@"; do
		target "$gw"
		sample_start "s2c-$gw"
		for c in 1 10 50 100; do
			lg "s2c-fast-$c-$gw" "$gw" openai oai fast -concurrency $c -prompt 16 -max-tokens 16 -duration 20s -warmup 5s -timeout 10s
		done
		for c in 1 10 50; do
			lg "s2c-tok10-$c-$gw" "$gw" openai oai tok10 -stream -expect 50 -concurrency $c -prompt 16 -duration 20s -warmup 5s -timeout 30s
			lg "s2c-tok0-$c-$gw" "$gw" openai oai tok0 -stream -expect 50 -concurrency $c -prompt 16 -duration 20s -warmup 5s -timeout 30s
		done
		sample_stop
	done
	down
}

# common: a rate all three sustain at 1 vCPU with headroom (70% of the slowest), for the headline.
common() {
	local m
	m=$(($(slowest 1) * 7 / 10 / 5 * 5))
	echo $((m > 10 ? m : 10))
}

headline() { # the fixed-rate points repeated REPEATS times, gateway order rotated each time
	local k i gw c order
	c=$(common)
	log "headline rates: 100 and $c rps non-stream, 10 and 50 streams/s"
	for ((k = 1; k <= REPEATS; k++)); do
		local targets=(direct "${ALL[@]}")
		order=()
		for ((i = 0; i < ${#targets[@]}; i++)); do order+=("${targets[(i + k) % ${#targets[@]}]}"); done
		for gw in "${order[@]}"; do
			target "$gw"
			sample_start "headline-$gw-r$k"
			point "s2-fixed-100-$gw-r$k" "$gw" openai oai fast -rate 100 -duration 30s
			point "s2-fixed-c-$gw-r$k" "$gw" openai oai fast -rate "$c" -duration 30s
			point "s3-chat-10-$gw-r$k" "$gw" openai oai chat -stream -expect 100 -rate 10 -duration 30s
			point "s3-chat-50-$gw-r$k" "$gw" openai oai chat -stream -expect 100 -rate 50 -duration 30s
			point "s5-cc-10-$gw-r$k" "$gw" anthropic anth chat -stream -expect 100 -system 12000 -tools 25 -turns 10 -prompt 2000 -rate 10 -duration 30s
			point "s5-cc-50-$gw-r$k" "$gw" anthropic anth chat -stream -expect 100 -system 12000 -tools 25 -turns 10 -prompt 2000 -rate 50 -duration 30s
			sample_stop
		done
	done
	down
}

s3() { # streams held open (first, on a fresh gateway), then more streams per second until it breaks
	local gw n r
	for gw in direct "$@"; do
		target "$gw"
		sample_start "s3-$gw"
		# long streams last ~27 s, so rate = n / 27.3 holds n open; measure once the ramp is done.
		# Anthropic format, which every gateway streams.
		for n in 1000 5000 10000; do
			lg "s3-open-$n-$gw" "$gw" anthropic anth long -stream -expect 10 -rate $((n * 10 / 273)) -warmup 30s -duration 30s -timeout 60s
			failed "$OUT/s3-open-$n-$gw.json" && break
			sleep 35 # let the last streams finish
		done
		for r in 200 500 1000; do
			point "s3-chat-$r-$gw" "$gw" openai oai chat -stream -expect 100 -rate "$r" -duration 30s -timeout 30s
			unsupported "$gw" openai oai -stream || ! failed "$OUT/s3-chat-$r-$gw.json" || break
		done
		sample_stop
	done
	down
}

s2x() { # non-stream with many requests in flight, over HTTPS, payload sizes and translation
	local gw r s
	for gw in direct "$@"; do
		target "$gw"
		sample_start "s2x-$gw"
		# 2.3 s responses, so 1000 rps holds ~2300 in flight; skip rates far beyond the gateway
		for r in 100 500 1000; do
			[[ $gw == direct ]] || ((r <= 2 * $(max "$gw"))) || continue
			point "s2-chat-$r-$gw" "$gw" openai oai chat -rate "$r" -duration 30s -timeout 30s
		done
		point "s2-tls-$gw" "$gw" openai tls chat -rate 50 -duration 30s -timeout 30s
		for s in 2000 100000 1000000; do point "s4-$s-$gw" "$gw" openai oai fast -prompt "$s" -rate 20 -duration 30s; done
		if [[ $gw != direct ]]; then
			point "s5-o2a-$gw" "$gw" openai anth chat -stream -expect 100 -rate 10 -duration 30s
			point "s5-a2o-$gw" "$gw" anthropic oai chat -stream -expect 100 -rate 10 -duration 30s
		fi
		sample_stop
	done
	down
}

s6() { # CPU scaling: the ladder again at 2 and 4 vCPU (LiteLLM gets as many workers)
	local gw c
	for gw in "$@"; do
		fresh "$gw"
		for c in 2 4; do
			start "$gw" "$c"
			wait_key "$gw"
			sample_start "s6-$c-$gw"
			ladder "$gw"
			sample_stop
		done
	done
	down
}

s7() { # resilience
	local gw f
	for gw in "$@"; do
		fresh "$gw"
		sample_start "s7-$gw"
		if [[ $gw != lrust ]]; then # no fallbacks in the Rust gateway yet
			for f in fo500 fo429 foflaky; do lg "s7-$f-$gw" "$gw" openai fo "$f" -rate 20 -duration 30s -warmup 0s -timeout 30s; done
			lg "s7-fohang-$gw" "$gw" openai fo fohang -rate 2 -duration 20s -warmup 0s -timeout 30s
		fi
		# Hang up mid-stream and before the first byte; 2 s later, is the upstream call still open?
		AFTER=2 point "s7-cancel-stream-$gw" "$gw" anthropic anth long -stream -cancel-tokens 2 -rate 20 -duration 10s -warmup 0s
		sleep 30
		AFTER=2 point "s7-cancel-slow-$gw" "$gw" openai oai slow -cancel-at 2s -rate 20 -duration 10s -warmup 0s
		sleep 30
		# 30 s before the first byte at 200 rps: ~6000 requests waiting on the upstream at once
		lg "s7-slow-$gw" "$gw" openai oai slow -rate 200 -duration 60s -warmup 0s -timeout 45s
		# A 429 from upstream: what status and Retry-After does the client get?
		curl -sS -D - -o /dev/null -H "Authorization: Bearer $(cat "$OUT/.key-$gw")" -H 'Content-Type: application/json' \
			-d "{\"model\":\"$(model "$gw" oai err429)\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
			"http://127.0.0.1:$(port "$gw")$([[ $gw == otari ]] && echo /api)/v1/chat/completions" > "$OUT/s7-429-$gw.txt" || true
		sleep 20
		sample_stop
	done
	down
}

s9() { # what budgets, limits and request logging cost, and whether the limits hold
	local gw rate
	for gw in "$@"; do
		[[ $gw == lrust ]] && continue # no keys, budgets, limits or usage logging to cost
		rate=$(($(max "$gw") / 2))
		fresh "$gw"
		sample_start "s9-$gw"
		point "s9-default-$gw" "$gw" openai oai fast -rate "$rate" -duration 60s
		[[ $gw == otari ]] && otari_rule '{"name":"limits","per":"key","rpm":100000000,"tpm":2000000000}'
		KEY=$gw-limits point "s9-limits-$gw" "$gw" openai oai fast -rate "$rate" -duration 60s
		[[ $gw == otari ]] && otari_rule -limits && otari_rule '{"name":"rpm","per":"key","rpm":600}'
		# 600 rpm allowed, 1200 rpm sent: how many get through?
		KEY=$gw-rpm lg "s9-rpm-$gw" "$gw" openai oai fast -rate 20 -duration 60s -warmup 0s
		[[ $gw == otari ]] && otari_rule -rpm
		# a $0.01 budget at ~$0.0007 a request: how many get through?
		KEY=$gw-budget lg "s9-budget-$gw" "$gw" openai oai fast -rate 10 -duration 30s -warmup 0s
		if [[ $gw == litellm ]]; then
			sed 's/# disable_spend_logs: true.*/disable_spend_logs: true/' litellm.yaml > "$OUT/litellm.min.yaml"
			LITELLM_CONFIG=$OUT/litellm.min.yaml start litellm 1
			wait_key litellm
			lg "s9-minimal-$gw" "$gw" openai oai fast -rate "$rate" -duration 60s
		fi
		sample_stop
	done
	down
}

s8() { # soak and spike at 2 vCPU: same absolute load for all, 70% non-stream, 30% Anthropic streams
	local gw r m seg bg
	r=$(($(slowest 2) / 2))
	log "soak rate: $r rps for $SOAK_MIN min"
	for gw in "$@"; do
		fresh "$gw"
		start "$gw" 2
		wait_key "$gw"
		sample_start "s8-$gw"
		for ((seg = 1; seg <= SOAK_MIN / 5; seg++)); do
			NOMOCK=1 lg "s8-fast-$gw-$seg" "$gw" openai oai fast -rate $((r * 7 / 10)) -duration 300s -warmup 0s &
			bg=$!
			NOMOCK=1 lg "s8-chat-$gw-$seg" "$gw" anthropic anth chat -stream -expect 100 -rate $((r * 3 / 10 > 1 ? r * 3 / 10 : 1)) -duration 300s -warmup 0s
			wait "$bg" # not a bare wait: the sampler is a child too, and never exits
		done
		# spike to twice this gateway's own maximum, then watch it recover in 10 s slices
		m=$(max "$gw" 2)
		lg "s8-pre-$gw" "$gw" openai oai fast -rate "$r" -duration 60s -warmup 0s
		lg "s8-spike-$gw" "$gw" openai oai fast -rate $((m * 2)) -duration 30s -warmup 0s -timeout 10s
		for seg in $(seq -w 1 18); do lg "s8-post-$gw-$seg" "$gw" openai oai fast -rate "$r" -duration 10s -warmup 0s -timeout 10s; done
		sample_stop
	done
	down
}

k6check() { # the same run through k6, to cross-check the load generator
	local gw=router
	fresh "$gw"
	lg "k6x-loadgen-$gw" "$gw" openai oai fast -rate 1000 -duration 30s
	dc run --rm --no-deps -T k6 run -q -e URL="$(url $gw openai oai)" -e KEY="$(cat "$OUT/.key-$gw")" -e MODEL=fast -e RATE=1000 \
		-e OUT="/results/$RUN/k6x-k6-$gw.json" /k6.js >/dev/null
	log "k6 http_req_duration: $(jq -c '.metrics.http_req_duration.values' "$OUT/k6x-k6-$gw.json")"
	log "loadgen service_ms:   $(jq -c '.service_ms | {p50, p90, p99, max}' "$OUT/k6x-loadgen-$gw.json")"
	down
}

# awake: on macOS, refuse to start on battery and keep the machine awake for the whole run. A
# closed lid still sleeps it, so slept checks after every scenario and marks the run INVALID.
awake() {
	[[ $(uname) == Darwin ]] || return 0
	if pmset -g batt | grep -q "'Battery Power'" && [[ -z ${ALLOW_BATTERY:-} ]]; then
		echo "on battery: plug in first (or ALLOW_BATTERY=1); a long run drains it and the Mac sleeps" >&2
		exit 1
	fi
	[[ -n ${CAFFEINATED:-} ]] || exec env CAFFEINATED=1 caffeinate -dims "$SELF" all
	log "keep the lid open until it's done: caffeinate can't stop lid-close sleep (sudo pmset -a disablesleep 1 can)"
}
slept() { [[ $(uname) == Darwin ]] && pmset -g log | awk -v t="$1" '($1 " " $2) > t && /Entering Sleep/ { s = 1 } END { exit !s }'; }

# all: every scenario, each in its own process so one failing doesn't stop the rest.
all() {
	local sc since
	awake
	since=$(date '+%Y-%m-%d %H:%M:%S')
	log "run $RUN (${ALL[*]}): $(podman machine inspect --format '{{.Resources.CPUs}} vCPU {{.Resources.Memory}} MiB' 2>/dev/null || echo "$(nproc) CPUs, $(uname -r)"), $(pmset -g batt 2>/dev/null | head -1)"
	build
	[[ " ${ALL[*]} " != *" lrust "* ]] || podman image exists "localhost/litellm-gateway:${LRUST_COMMIT:0:8}" || build_lrust
	for sc in cal s1 s2 k6check s2c headline s3 s2x s7 s9 s6 s8; do
		log "== $sc"
		"$SELF" "$sc" || log "!! $sc failed (exit $?)"
		if [[ ! -f $OUT/INVALID ]] && slept "$since"; then
			log "!! the machine slept during $sc: this run's results are invalid"
			touch "$OUT/INVALID"
		fi
	done
	down
	log "done"
}

# Sourced (real.sh does), only define the functions.
[[ ${BASH_SOURCE[0]} == "$0" ]] || return 0

cmd=${1:-}
shift || true
case $cmd in
build | all | cal | headline | k6check | down) "$cmd" ;;
build-lrust) build_lrust ;;
s1 | s2 | s2c | s2x | s3 | s6 | s7 | s8 | s9) "$cmd" "${@:-${ALL[@]}}" ;;
real) "$PWD/real.sh" ;;
report) go run ./loadgen report "$OUT" ;;
docs)
	[[ ! -f $OUT/INVALID || -n ${FORCE:-} ]] || { echo "$OUT is marked INVALID (the machine slept during it); FORCE=1 uses it anyway" >&2; exit 1; }
	go run ./loadgen summary "$OUT" ../docs/benchmarks.md
	;;
call) "$@" ;; # any function, e.g. ./run.sh call fresh router
*) sed -n '2,16p' "$SELF" && exit 2 ;;
esac; exit # on the esac line: bash reads scripts as it runs them, so nothing after it is read
