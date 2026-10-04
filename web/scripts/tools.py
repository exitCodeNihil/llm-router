"""Round-trip tool calling through the gateway on every path it supports."""
import json, subprocess, sys, urllib.request

GW = "http://localhost:8080"
KEY = open("/tmp/cc.txt").read().strip()
OAUTH = json.loads(subprocess.run(
    ["security", "find-generic-password", "-s", "Claude Code-credentials", "-w"],
    capture_output=True, text=True).stdout)["claudeAiOauth"]["accessToken"]

OAI_TOOLS = [{"type": "function", "function": {
    "name": "get_weather",
    "description": "Get the current weather for a city.",
    "parameters": {"type": "object", "properties": {
        "city": {"type": "string", "description": "City name"},
        "unit": {"type": "string", "enum": ["c", "f"]}}, "required": ["city"]}}}]

ANTH_TOOLS = [{"name": "get_weather", "description": "Get the current weather for a city.",
               "input_schema": {"type": "object", "properties": {
                   "city": {"type": "string"}, "unit": {"type": "string", "enum": ["c", "f"]}},
                   "required": ["city"]}}]

def post(path, body, headers, stream=False):
    req = urllib.request.Request(GW + path, data=json.dumps(body).encode(),
                                 headers={"content-type": "application/json", **headers})
    with urllib.request.urlopen(req, timeout=120) as r:
        raw = r.read().decode()
    if not stream:
        return json.loads(raw)
    events = []
    for line in raw.splitlines():
        if line.startswith("data: ") and line[6:].strip() != "[DONE]":
            try: events.append(json.loads(line[6:]))
            except Exception: pass
    return events

def oai_headers():  return {"Authorization": "Bearer " + KEY}
def sub_headers():  return {"X-Llmr-Key": KEY, "Authorization": "Bearer " + OAUTH,
                            "anthropic-version": "2023-06-01"}

results = []
def check(name, ok, detail=""):
    results.append(ok)
    print(f"  {'ok  ' if ok else 'FAIL'} {name}" + (f" — {detail}" if detail else ""))

def oai_roundtrip(model, headers, stream):
    """OpenAI surface: ask, expect a tool_call, answer it, expect a final reply."""
    msgs = [{"role": "user", "content": "What is the weather in Paris? Use the tool."}]
    body = {"model": model, "messages": msgs, "tools": OAI_TOOLS, "max_tokens": 300}
    label = f"{model} [{'stream' if stream else 'buffered'}]"
    if stream:
        evs = post("/v1/chat/completions", {**body, "stream": True}, headers, stream=True)
        name, args = "", ""
        for e in evs:
            for c in e.get("choices", []):
                for tc in (c.get("delta") or {}).get("tool_calls") or []:
                    fn = tc.get("function") or {}
                    name += fn.get("name") or ""
                    args += fn.get("arguments") or ""
        if not name:
            return check(f"{label}: emits a tool call", False, "no tool_calls in stream")
        check(f"{label}: emits a tool call", True, f"{name}({args[:40]})")
        try:
            parsed = json.loads(args)
            check(f"{label}: tool arguments are valid JSON", "city" in parsed, args[:60])
        except Exception as e:
            check(f"{label}: tool arguments are valid JSON", False, f"{e}: {args[:60]}")
        return
    r = post("/v1/chat/completions", body, headers)
    m = r["choices"][0]["message"]
    tcs = m.get("tool_calls") or []
    if not tcs:
        return check(f"{label}: emits a tool call", False, str(m.get("content"))[:70])
    fn = tcs[0]["function"]
    check(f"{label}: emits a tool call", True, f"{fn['name']}({fn['arguments'][:36]})")
    try:
        parsed = json.loads(fn["arguments"])
        check(f"{label}: tool arguments are valid JSON", "city" in parsed, fn["arguments"][:60])
    except Exception as e:
        return check(f"{label}: tool arguments are valid JSON", False, str(e))
    # feed the result back
    msgs += [m, {"role": "tool", "tool_call_id": tcs[0]["id"], "content": '{"temp_c": 18, "sky": "clear"}'}]
    r2 = post("/v1/chat/completions", {**body, "messages": msgs}, headers)
    final = (r2["choices"][0]["message"].get("content") or "")
    check(f"{label}: uses the tool result", "18" in final or "clear" in final.lower(), final[:70])

def anth_roundtrip(model, headers, stream):
    """Anthropic surface, the shape Claude Code actually sends."""
    msgs = [{"role": "user", "content": "What is the weather in Paris? Use the tool."}]
    body = {"model": model, "max_tokens": 300, "tools": ANTH_TOOLS, "messages": msgs}
    if "X-Llmr-Key" in headers:
        # Subscription OAuth tokens are gated on a first-party identity block;
        # without it Anthropic answers 429, which looks like a quota limit.
        body["system"] = [{"type": "text",
                           "text": "You are Claude Code, Anthropic's official CLI for Claude."}]
    label = f"{model} [/v1/messages {'stream' if stream else 'buffered'}]"
    if stream:
        evs = post("/v1/messages", {**body, "stream": True}, headers, stream=True)
        name, args = "", ""
        for e in evs:
            if e.get("type") == "content_block_start" and e.get("content_block", {}).get("type") == "tool_use":
                name = e["content_block"]["name"]
            if e.get("type") == "content_block_delta" and e.get("delta", {}).get("type") == "input_json_delta":
                args += e["delta"].get("partial_json", "")
        if not name:
            return check(f"{label}: emits tool_use", False, "no tool_use block")
        check(f"{label}: emits tool_use", True, f"{name}({args[:40]})")
        try:
            check(f"{label}: tool input is valid JSON", "city" in json.loads(args), args[:60])
        except Exception as e:
            check(f"{label}: tool input is valid JSON", False, f"{e}: {args[:60]}")
        return
    r = post("/v1/messages", body, headers)
    tu = [b for b in r.get("content", []) if b.get("type") == "tool_use"]
    if not tu:
        return check(f"{label}: emits tool_use", False, json.dumps(r.get("content"))[:80])
    check(f"{label}: emits tool_use", True, f"{tu[0]['name']}({json.dumps(tu[0]['input'])[:36]})")
    check(f"{label}: tool input is valid JSON", "city" in tu[0]["input"], json.dumps(tu[0]["input"])[:60])
    msgs += [{"role": "assistant", "content": r["content"]},
             {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tu[0]["id"],
                                           "content": '{"temp_c": 18, "sky": "clear"}'}]}]
    r2 = post("/v1/messages", {**body, "messages": msgs}, headers)
    text = " ".join(b.get("text", "") for b in r2.get("content", []))
    check(f"{label}: uses the tool result", "18" in text or "clear" in text.lower(), text[:70])

which = sys.argv[1] if len(sys.argv) > 1 else "all"
if which in ("all", "lmstudio"):
    print("\n── LM Studio (openai flavour, passthrough) ──")
    for st in (False, True):
        try: oai_roundtrip("local-chat", oai_headers(), st)
        except Exception as e: check(f"local-chat [{st}]", False, str(e)[:90])
if which in ("all", "foundry"):
    print("\n── Foundry / Azure (anthropic flavour, OpenAI→Anthropic conversion) ──")
    for st in (False, True):
        try: oai_roundtrip("claude-haiku", oai_headers(), st)
        except Exception as e: check(f"claude-haiku [{st}]", False, str(e)[:90])
    try: anth_roundtrip("claude-haiku", oai_headers(), False)
    except Exception as e: check("claude-haiku native", False, str(e)[:90])
if which in ("all", "sub"):
    print("\n── Claude subscription proxy (oauth_passthrough, native) ──")
    for st in (False, True):
        try: anth_roundtrip("claude-sonnet-5", sub_headers(), st)
        except Exception as e: check(f"claude-sonnet-5 [{st}]", False, str(e)[:90])

print(f"\n{sum(results)}/{len(results)} tool-calling checks passed")
