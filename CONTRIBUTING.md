# Contributing

Bug reports and pull requests are welcome. For anything larger than a fix, open an issue
first so we can agree on the shape.

```bash
make dev       # gateway with live reload + console with HMR (starts Postgres if needed)
make check     # what CI runs: go vet, go test, console typecheck
make verify    # browser suites against a running console (needs `make dev`)
```

- **Migrations are forward-only.** Add `internal/store/migrations/NNNN_name.sql`; never edit
  one that has shipped.
- **`web/dist` is committed** because `web/embed.go` bakes it into the binary and
  `go build` must work on a fresh clone. After console changes run `make web` and commit
  the result.
- Keep changes small and tested; the hot path never touches the database, so keep it that way.
- Console styling lives in `web/src/theme.ts` only (see [web/README.md](web/README.md)).

By contributing you agree your work is licensed under the Apache License 2.0.
