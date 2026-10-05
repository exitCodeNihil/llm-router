# Console

The web console the gateway serves at `/`. React 19 and [Astryx](https://astryx.atmeta.com)
(Meta's MIT-licensed design system), built with Vite and embedded into the Go binary by
`web/embed.go`.

## Develop

From the repository root:

```bash
make dev        # gateway (live reload) + console (Vite HMR) on http://localhost:5173
make dev-web    # console only, against an already-running gateway
```

Vite proxies `/api`, `/auth` and `/v1` to the gateway; point it elsewhere with
`LLMR_DEV_BACKEND=http://host:port`.

## Layout

```
src/
  theme.ts       design tokens, the single source of visual truth
  lib/           API client, types, react-query hooks, SSE streaming
  components/    shared building blocks (Shell, Page, charts, dialogs, forms)
  pages/         one file per route
scripts/         Playwright suites (`make verify`, `make audit`)
```

## Theming

Edit `src/theme.ts` and nothing else. `make theme` compiles it to static CSS (it runs before `dev`
and `build`); the generated `theme.built.css` and `llm-router.*` files are gitignored.

## Build

```bash
make web        # console -> web/dist
make build      # console + Go binary
```

`web/dist` is committed so `go build` works on a fresh clone without Node. Rebuild and commit it
whenever the console changes.
