# Workspace templates

The Dockerfiles here are embedded into the binary and seeded into the
`workspace_templates` table at startup, where admins edit them and build them
from the console (Workspaces → Templates). Edit a shipped template in the console
and it stops following this directory; delete the row to get the shipped one back.

To build by hand instead:

```bash
podman build -t llmr-workspace-base -f base.Dockerfile .
```

Nothing here is special — a workspace image just needs `git`, `tar`, `pi` and
`code-server` on `$PATH`, and must start code-server on port 8080.

The image's `CMD` runs code-server with `--auth none`: it is reachable only
through the gateway's proxy, which checks your session and that you own the
workspace. On Docker the port is published to `127.0.0.1` only, or not at all when
`LLMR_WORKSPACE_NETWORK` is set; on Kubernetes it is never published.

## The one rule

`ENV HOME=/workspace/.home SHELL=/usr/bin/zsh` and `WORKDIR /workspace`, with `zsh`
installed (the IDE terminal opens `$SHELL`).

`/workspace` is the only path on the volume, so it is the only path that
survives the container being recreated. Putting `$HOME` inside it is what keeps
pi's chat sessions, your uploaded ssh keys and your git config across restarts.

Everything else lives in the container layer and is lost on recreate — including
anything you `apt install` from the workspace terminal. Add tools to the
Dockerfile instead.
