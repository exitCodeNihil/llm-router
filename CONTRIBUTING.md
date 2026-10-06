# Contributing

Bug reports and pull requests are welcome. For anything bigger than a fix, open an issue first.

## Development

```bash
make dev       # gateway (live reload) + console (HMR); starts Postgres if needed
make check     # what CI runs: go vet, go test, console typecheck
make verify    # browser suites against a running `make dev`
make build     # single binary with the console embedded
make help      # all targets
```

`make dev` serves the console on <http://localhost:5173> and the gateway on
<http://localhost:8080>. Override defaults inline, e.g. `make dev PORT=9000
LLMR_DATABASE_URL=postgres://…`. `make stack` also starts Zitadel (OIDC) and Langfuse for SSO and
export work. Container tooling prefers podman and falls back to docker (`ENGINE=docker` to force).

## Conventions

- **Architecture**: one binary. `--mode=all` is the control plane (management API, console) plus
  gateway on Postgres; `--mode=gateway` is a stateless edge node that syncs a signed config snapshot.
  The hot path never touches the database: config lives in an atomically swapped in-memory
  snapshot. Keep it that way.
- **Migrations are forward-only.** Add `internal/store/migrations/NNNN_name.sql`; never edit one
  that has shipped. The gateway applies them at startup but doesn't create the database itself.
  `make db-reset` drops the schema so the next start replays everything.
- **`web/dist` is committed** because `web/embed.go` bakes it into the binary and `go build` must
  work on a fresh clone. After console changes run `make web` and commit the result.
- Console styling lives in `web/src/theme.ts` only (see [web/README.md](web/README.md)).

## Releases

Releases are automatic ([Release Please](https://github.com/googleapis/release-please)). Title PRs
as conventional commits and squash-merge them: `fix:` is a patch, `feat:` a patch before 1.0 and a
minor after, and `chore:`, `docs:`, `ci:` and `test:` don't release. A release PR collects them
into the changelog and the version (including the Helm chart). Merging it tags the release and
publishes the image. Don't edit `CHANGELOG.md` or tag by hand.

By contributing you agree your work is licensed under the Apache License 2.0.
