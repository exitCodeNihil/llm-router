<!--
Title the pull request as a conventional commit, for example `fix(gateway): ...` or `feat: ...`.
It becomes the changelog entry when the pull request is squash-merged.
For anything bigger than a fix, please open an issue first.
-->

## What changes

<!-- What this does and why, in a few sentences. Link the issue if there is one: Fixes #123 -->

## How it was tested

<!-- Commands you ran, or what you checked by hand. -->

## Checklist

- [ ] `make check` passes (and `cargo test` in `gateway/` if the gateway changed)
- [ ] Tests cover the change, or it needs none
- [ ] Docs are updated if behaviour or configuration changed
- [ ] `web/dist` is rebuilt (`make web`) if the console changed
- [ ] New migrations are added as new files; shipped ones are not edited
