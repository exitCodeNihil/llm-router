# llm-router console

The web console served by the gateway at `/`. React 19 + [Astryx](https://astryx.atmeta.com)
(Meta's open-source design system, MIT), built with Vite and embedded into the Go
binary by `web/embed.go`.

## Development

From the repository root:

```bash
make dev        # gateway (air live reload) + console (Vite HMR)
```

- console → http://localhost:5173 (hot reload)
- gateway → http://localhost:8080

Vite proxies `/api`, `/auth` and `/v1` to the gateway. Point it elsewhere with
`LLMR_DEV_BACKEND=http://host:port`.

Console-only (against an already-running gateway):

```bash
make dev-web
```

## Layout

```
src/
  theme.ts          design tokens — the single source of visual truth
  app.css           document-level rules only; everything else is a component
  lib/              API client, types, react-query hooks, SSE streaming
  components/       app-level building blocks (Shell, Page, charts, dialogs, forms)
  pages/            one file per route; chat/ is a folder (see below)
scripts/            Playwright verification suites
```

`pages/chat/` is split because the playground carries real logic:

| file | responsibility |
|---|---|
| `wire.ts` | pure request shaping — no React, independently testable |
| `useChatSession.ts` | transcript, streaming turn, tool loop, persistence |
| `parts.tsx` | presentation, driven entirely by props |
| `index.tsx` | composition + responsive layout |

## Theming

`src/theme.ts` maps the product's design language (cool slate base, single amber
"signal" accent, monospace for data) onto Astryx tokens with
`defineTheme({ extends: neutralTheme })`. Colours are `[light, dark]` tuples
compiled to CSS `light-dark()`.

`npm run theme` (or `make theme`) compiles it to static CSS via
`astryx theme build`, so no runtime colour maths ships in the bundle. It runs
automatically before `dev` and `build`. The generated files
(`theme.built.css`, `llm-router.*`) are gitignored — never edit them.

**To restyle the console, edit `src/theme.ts` and nothing else.**

## Conventions

The Astryx rules that matter:

- No raw `<div>` for layout — use `VStack` / `HStack` / `Grid` / `Section`.
- No hardcoded colours or pixel values — use `var(--color-*)`, `var(--spacing-*)`.
- Dense data is rows (`Table`, `List`); `Card` is for dashboard widgets and
  settings groups, never as a list-item wrapper.
- Discover components with `npm run astryx -- component <Name>` before writing UI.

## Verification

Three Playwright suites, run against a live gateway with real data:

```bash
make verify     # all three
make audit      # routes × viewports only (fast)
```

| suite | what it checks |
|---|---|
| `scripts/audit.mjs` | every route × 3 viewports × colour mode: console errors, failed requests, horizontal overflow, main landmark, screenshots |
| `scripts/interact.mjs` | dialog focus management, focus trap, Escape, focus restore, validation gating, combobox ARIA, colour mode |
| `scripts/e2e.mjs` | create → verify → delete an API key through the UI, one-time secret semantics, a streaming chat turn |
| `scripts/crud.mjs` | breadth sweep: create/edit/delete for users, teams, providers, deployments; pricing and ops pages render |
| `scripts/chat.mjs` | playground depth: conversation persistence, attachments, tool editing, structured output |
| `scripts/attach.mjs` | composer inputs: clipboard paste (image + long text), drag & drop, model switcher, image round-trip |
| `scripts/analytics.mjs` | analytics: filtered aggregates, breakdown dimensions, timeseries, drill-down into Requests, Langfuse deep-link |
| `scripts/guards.mjs` | consequential switches confirm before applying, cancel is a no-op, capture policy stays a draft |
| `scripts/obs.mjs` | observability: capture-policy draft/apply/discard, pending diff, team/user/key rule scoping, export delivery health, credential test |
| `scripts/insights.mjs` | operator readouts: routing attribution, TTFT/latency/cost, request-id, copy/regenerate, send-as-key policy, context meter, curl export |

`audit.mjs` takes `AUDIT_MODE=dark|light|both`. Screenshots land in `/tmp/astryx-shots`.

The playground suite needs a real password user, because bootstrap-token sessions
have no user row and therefore cannot persist conversations:

```bash
make verify-chat LLMR_USER=you@example.com LLMR_PASS=…
```

All suites are **non-destructive**: each creates its own records, identified by a
unique tag, and removes them afterwards. Pre-existing data is never touched.

## Build

```bash
make web        # console → web/dist
make build      # console + Go binary with dist embedded
```

`web/dist` is committed so `go build` works on a fresh clone without Node.
Rebuild and commit it whenever the console changes.
