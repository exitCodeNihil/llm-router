# Connect your tools

You need the gateway URL, a key (**API Keys → New key**) and a model name from **Models**. The
key's **Connect** button shows ready-to-paste config for Claude Code, OpenCode, pi and curl, with
your models filled in. Examples below use `http://localhost:8080`.

## Claude Code

### On your claude.ai subscription

Claude Code keeps its own login. The gateway forwards it untouched and records every request;
no credential is stored.

1. The quickstart adds an `anthropic-subscription` provider with one model, `claude-*`, which passes
   any Claude model name through, so new Claude models work without changes. To add it by hand:
   **Providers → Add provider**, type *OpenAI-compatible*, Base URL `https://api.anthropic.com`,
   Auth mode *Forward the caller's token*; then **Models → Add model** with `claude-*` as both the
   model and upstream name.
2. Point Claude Code at the gateway. The key goes in a header so the subscription token stays in
   `Authorization`:

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_CUSTOM_HEADERS="X-Llmr-Key: llmr_…"
claude
```

- Do not set `ANTHROPIC_AUTH_TOKEN`: it replaces your subscription token and Anthropic rejects the
  gateway key.
- Spend reads $0 and budgets don't apply (rate limits do). Watch tokens and cache hits instead.
- A 429 with an empty message usually means Anthropic didn't recognise the request, not that your
  quota is used up.
- An exact model name wins over `claude-*`: if another provider also serves, say,
  `claude-sonnet-5-5`, requests for that name go there. Give such models their own names.
- Use your own subscription with your own Claude Code. It is not a way to share one subscription
  across a team; check Anthropic's terms.

### On other providers

Any model the gateway serves (Claude on Vertex or Foundry, Gemini, OpenRouter, vLLM) works in Claude
Code with a gateway key:

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=llmr_…
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1   # /model lists your key's models
export ANTHROPIC_MODEL=my-model
claude
```

Discovery lists only names containing `claude` or `anthropic`. Other names work if you type them,
or map them onto Claude Code's model slots:

```bash
export ANTHROPIC_DEFAULT_OPUS_MODEL=big-model
export ANTHROPIC_DEFAULT_SONNET_MODEL=my-model
export ANTHROPIC_DEFAULT_HAIKU_MODEL=small-model      # background tasks
```

- Claude upstreams (Anthropic, Vertex, Foundry) get requests unchanged: thinking, prompt caching
  and tools all work.
- Other upstreams get requests translated to chat completions. Text, base64 images, tools and
  streaming work; `thinking`, `cache_control` and document blocks are dropped.
- A slot you don't map asks for Claude Code's own default name, and fails unless the gateway has a
  model by that name.
- Claude Code warns about names it doesn't recognise and assumes a 200k-token context. Append `[1m]`
  (`ANTHROPIC_MODEL=my-model[1m]`) if the model takes 1M; the suffix is stripped before the request.

## Other clients

OpenAI-compatible clients take base URL `http://localhost:8080/v1` and your key as the API key.

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="llmr_…")
client.chat.completions.create(model="my-model", messages=[{"role": "user", "content": "hi"}])
```

```python
import anthropic
client = anthropic.Anthropic(base_url="http://localhost:8080", api_key="llmr_…")
client.messages.create(model="my-model", max_tokens=256, messages=[{"role": "user", "content": "hi"}])
```

- **OpenCode, pi**: read a static model list, so use the key's **Connect** button to generate
  `opencode.json` or `~/.pi/agent/models.json`.
- **Cursor**: Settings → Models → OpenAI API Key, enable *Override OpenAI Base URL*. Not tested
  here; Cursor may need a publicly reachable URL.
- **Roo Code, Cline**: provider *OpenAI Compatible* with the base URL and key above.
