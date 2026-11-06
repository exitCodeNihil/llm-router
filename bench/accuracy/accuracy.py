#!/usr/bin/env python3
"""Checks what llm-router records against what Anthropic really reports and bills.

Sends real requests through each gateway, takes Anthropic's own usage object as the
truth, prices it from the table below (typed from the pricing page, independent of
the gateway), and compares that with the usage_events row and the Langfuse generation.
Spends real money: about $2 for a full run.

  ANTHROPIC_TOKEN=...  API key for api.anthropic.com
  LLMR_ADMIN_TOKEN=... the control plane's admin token
  LLMR_URL=http://127.0.0.1:8080          the control plane
  LLMR_GATEWAY_URL=http://127.0.0.1:8081  a Rust gateway too (optional)
  LLMR_PSQL="psql postgres://..."         or "podman exec db psql -U u -d d"
  LANGFUSE_URL=http://localhost:3001 LANGFUSE_KEYS=pk:sk  also check Langfuse (optional)
  LLMR_PROVIDER_ID=...  test this existing provider with no models yet, say a workload identity one,
                        instead of creating one with ANTHROPIC_TOKEN

  accuracy.py [models] [cache] [options] [long]    exits 1 on any mismatch
"""
import base64, json, os, shlex, subprocess, sys, time, urllib.error, urllib.request, uuid

# $/MTok: input, 5m cache write, 1h cache write, cache read, output.
# platform.claude.com/docs/en/about-claude/pricing
PUBLISHED = {
    "claude-fable-5-1": (10, 12.5, 20, 0.25, 50),
    "claude-fable-5": (10, 12.5, 20, 1, 50),
    "claude-opus-5-5": (4, 5, 8, 0.2, 20),
    "claude-opus-5": (5, 6.25, 10, 0.5, 25),
    "claude-opus-4-8": (5, 6.25, 10, 0.5, 25),
    "claude-opus-4-7": (5, 6.25, 10, 0.5, 25),
    "claude-opus-4-6": (5, 6.25, 10, 0.5, 25),
    "claude-opus-4-5-20251101": (5, 6.25, 10, 0.5, 25),
    "claude-sonnet-5-5": (2, 2.5, 4, 0.1, 10),
    "claude-sonnet-5": (2, 2.5, 4, 0.2, 10),
    "claude-sonnet-4-6": (3, 3.75, 6, 0.3, 15),
    "claude-sonnet-4-5-20250929": (3, 3.75, 6, 0.3, 15),
    "claude-haiku-5-5": (0.1, 0.125, 0.2, 0.01, 0.5),
    "claude-haiku-4-5-20251001": (1, 1.25, 2, 0.1, 5),
}
HAIKU_55_OVER_100K = (0.5, 0.625, 1, 0.05, 2.5)
CACHING = ["claude-haiku-5-5", "claude-sonnet-5-5", "claude-opus-5-5", "claude-haiku-4-5-20251001"]

CP = os.environ.get("LLMR_URL", "http://127.0.0.1:8080").rstrip("/")
GATEWAYS = {"control-plane": CP}
if os.environ.get("LLMR_GATEWAY_URL"):
    GATEWAYS["gateway"] = os.environ["LLMR_GATEWAY_URL"].rstrip("/")
ADMIN = os.environ["LLMR_ADMIN_TOKEN"]
PSQL = shlex.split(os.environ["LLMR_PSQL"])
LF = os.environ.get("LANGFUSE_URL", "").rstrip("/")
LF_AUTH = base64.b64encode(os.environ.get("LANGFUSE_KEYS", "").encode()).decode()


def api(method, path, body=None):
    req = urllib.request.Request(CP + path, method=method, data=None if body is None else json.dumps(body).encode(),
                                 headers={"Authorization": "Bearer " + ADMIN, "content-type": "application/json"})
    with urllib.request.urlopen(req) as f:
        raw = f.read()
        return json.loads(raw) if raw else None


def setup():
    """A throwaway provider, catalog-priced models and key; a run tests the catalog too."""
    name = "accuracy-" + uuid.uuid4().hex[:8]
    prov = {"id": os.environ["LLMR_PROVIDER_ID"]} if os.environ.get("LLMR_PROVIDER_ID") else api(
        "POST", "/api/providers", {"name": name, "type": "anthropic", "auth_mode": "api_key", "api_key": os.environ["ANTHROPIC_TOKEN"]})
    for m in PUBLISHED:
        api("POST", "/api/deployments", {"provider_id": prov["id"], "model_name": f"{name}/{m}", "upstream_name": m,
                                         "api_flavor": "anthropic", "catalog_model_id": "anthropic/" + m})
    team = api("POST", "/api/teams", {"name": name})
    key = api("POST", "/api/keys", {"name": name, "team_id": team["id"]})
    time.sleep(4)  # for the config to reach gateways
    return name, key["key"], prov["id"], team["id"], key["id"]


def post(base, path, body, key):
    headers = {"Authorization": "Bearer " + key, "content-type": "application/json", "anthropic-version": "2023-06-01"}
    if body.get("speed"):
        headers["anthropic-beta"] = "fast-mode-2026-02-01"
    req = urllib.request.Request(base + path, data=json.dumps(body).encode(), headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=300) as f:
            return f.status, f.headers, f.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read().decode()


def messages_usage(text, stream):
    if not stream:
        return json.loads(text)["usage"]
    usage = {}
    for line in text.splitlines():
        if line.startswith("data: {"):
            d = json.loads(line[6:])
            if d.get("type") == "message_start":
                usage.update(d["message"].get("usage") or {})
            elif d.get("type") == "message_delta":
                usage.update(d.get("usage") or {})
    return usage


def chat_usage(text, stream):
    u = {}
    if stream:
        for line in text.splitlines():
            if line.startswith("data: {") and json.loads(line[6:]).get("usage"):
                u = json.loads(line[6:])["usage"]
    else:
        u = json.loads(text)["usage"]
    cached = (u.get("prompt_tokens_details") or {}).get("cached_tokens", 0)
    return {"input_tokens": u.get("prompt_tokens", 0) - cached, "output_tokens": u.get("completion_tokens", 0),
            "cache_read_input_tokens": cached}


def truth(model, u):
    """Tokens and dollars by Anthropic's rules, from its own usage object."""
    fresh, out, read = u.get("input_tokens", 0), u.get("output_tokens", 0), u.get("cache_read_input_tokens", 0)
    write = u.get("cache_creation_input_tokens", 0)
    write_1h = (u.get("cache_creation") or {}).get("ephemeral_1h_input_tokens", 0)
    write_5m = write - write_1h
    prompt = fresh + read + write
    rates = HAIKU_55_OVER_100K if model == "claude-haiku-5-5" and prompt > 100_000 else PUBLISHED[model]
    i, w5, w1, r, o = rates
    usd = fresh * i + write_5m * w5 + write_1h * w1 + read * r + out * o
    usd *= (1.1 if u.get("inference_geo") == "us" else 1) * (2 if u.get("speed") == "fast" else 1)
    return {"prompt": prompt, "completion": out, "cached": read, "write_5m": write_5m, "write_1h": write_1h, "cost": usd / 1e6}


def small(**extra):
    return {"max_tokens": 24, "messages": [{"role": "user", "content": "Reply with the single word OK."}], **extra}


def cases(which):
    if "models" in which:
        for m in PUBLISHED:
            yield m, "messages", "/v1/messages", small()
            yield m, "messages stream", "/v1/messages", small(stream=True)
            yield m, "chat", "/v1/chat/completions", small()
            yield m, "chat stream", "/v1/chat/completions", small(stream=True, stream_options={"include_usage": True})
    if "cache" in which:
        for m in CACHING:
            for ttl in ("5m", "1h"):
                for stream in (False, True):
                    pad = (f"{uuid.uuid4()} " + "The quick brown fox jumps over the lazy dog near the river bank. ") * 700
                    control = {"type": "ephemeral", **({"ttl": "1h"} if ttl == "1h" else {})}
                    body = {**small(), "system": [{"type": "text", "text": pad, "cache_control": control}]}
                    if stream:
                        body["stream"] = True
                    label = f"{ttl}{' stream' if stream else ''}"
                    yield m, "cache write " + label, "/v1/messages", body
                    yield m, "cache read " + label, "/v1/messages", body
    if "options" in which:
        for stream in (False, True):
            s = {"stream": True} if stream else {}
            yield "claude-sonnet-5-5", "US-only", "/v1/messages", small(inference_geo="us", **s)
            yield "claude-opus-5-5", "fast mode", "/v1/messages", small(speed="fast", **s)
    if "long" in which:
        pad = "The quick brown fox jumps over the lazy dog near the river bank. " * 8200
        yield "claude-haiku-5-5", "over 100k prompt", "/v1/messages", {"max_tokens": 16, "messages": [{"role": "user", "content": pad + " Reply OK."}]}


def row(rid):
    out = subprocess.run(PSQL + ["-At", "-F", "|", "-c",
                                 f"select prompt_tokens,completion_tokens,cached_tokens,cache_write_5m_tokens,cache_write_1h_tokens,cost_usd from usage_events where request_id='{rid}'"],
                         capture_output=True, text=True).stdout.strip()
    if not out:
        return None
    p, c, r, w5, w1, usd = out.split("|")
    return {"prompt": int(p), "completion": int(c), "cached": int(r), "write_5m": int(w5), "write_1h": int(w1), "cost": float(usd)}


def langfuse(trace_id):
    # Langfuse v4 serves observations by trace id; its trace API is gone.
    url = f"{LF}/api/public/v2/observations?fields=core,basic,model,usage&traceId={trace_id}"
    req = urllib.request.Request(url, headers={"Authorization": "Basic " + LF_AUTH})
    for _ in range(8):
        try:
            with urllib.request.urlopen(req, timeout=20) as f:
                gens = [o for o in json.loads(f.read()).get("data", []) if o.get("type") == "GENERATION"]
                if gens:
                    return gens[0]
        except urllib.error.HTTPError:
            pass
        time.sleep(4)
    return None


def main():
    which = sys.argv[1:] or ["models", "cache", "options", "long"]
    prefix, key, *_ = setup()
    sent = []
    for gw, base in GATEWAYS.items():
        for model, kind, path, body in cases(which):
            stream = bool(body.get("stream"))
            status, headers, text = post(base, path, {**body, "model": f"{prefix}/{model}"}, key)
            if status != 200:
                print(f"FAIL {gw} {model} {kind}: HTTP {status} {text[:200]}")
                sent.append(None)
                continue
            u = messages_usage(text, stream) if path == "/v1/messages" else chat_usage(text, stream)
            sent.append((gw, model, kind, headers.get("X-Request-Id"), headers.get("X-Llmr-Trace-Id"), truth(model, u)))
    time.sleep(12)  # usage writes and gateway syncs
    bad = sent.count(None)
    checked = 0
    for s in filter(None, sent):
        gw, model, kind, rid, trace_id, want = s
        have = row(rid)
        problems = []
        if have is None:
            problems.append("no usage row")
        else:
            for field in ("prompt", "completion", "cached", "write_5m", "write_1h"):
                if have[field] != want[field]:
                    problems.append(f"{field} {have[field]} != {want[field]}")
            if abs(have["cost"] - want["cost"]) > 1e-9:
                problems.append(f"cost {have['cost']:.8f} != {want['cost']:.8f}")
            if LF:
                g = langfuse(trace_id)
                if g is None:
                    problems.append("no Langfuse generation")
                elif g.get("inputUsage") != have["prompt"] or g.get("outputUsage") != have["completion"] or abs((g.get("totalCost") or 0) - have["cost"]) > 1e-9:
                    problems.append(f"Langfuse {g.get('inputUsage')}/{g.get('outputUsage')}/{g.get('totalCost')}")
        checked += 1
        if problems:
            bad += 1
            print(f"FAIL {gw} {model} {kind}: " + "; ".join(problems))
    print(f"{checked} requests checked, {bad} mismatches")
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
