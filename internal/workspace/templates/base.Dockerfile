# Base workspace: git, a shell, and the pi agent. Node is here because pi is a
# Node CLI (it needs >= 22.19), not because the workspace is for Node work —
# use node.Dockerfile for that.
#
# Anything installed at runtime lives in the container layer and disappears when
# the container is recreated. Only /workspace is on the volume. Add tools here,
# not with `apt install` inside a running workspace.
FROM node:22-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates openssh-client curl less zsh \
 && rm -rf /var/lib/apt/lists/*

RUN npm install -g @earendil-works/pi-coding-agent

# The IDE. Editing, search, git, debugging and a real terminal — with a real
# PTY, so ctrl+C and vim work — all come from here rather than from anything we
# wrote. It listens on 8080; the gateway proxies to it behind session auth.
RUN curl -fsSL https://code-server.dev/install.sh | sh

# $HOME sits on the volume so pi's sessions, the ssh keys you upload and your
# git config all survive a restart. This is the whole persistence story.
#
# Setting HOME alone is not enough: OpenSSH expands "~" via getpwuid(), not
# $HOME, so it would look in /root and never find an uploaded key. Point the
# passwd entry at the volume too or git-over-ssh fails with no useful error.
# (usermod refuses to touch a live user, hence sed.)
RUN sed -i 's|^root:x:0:0:root:/root:.*|root:x:0:0:root:/workspace/.home:/usr/bin/zsh|' /etc/passwd
ENV HOME=/workspace/.home SHELL=/usr/bin/zsh
WORKDIR /workspace

# Bound to every interface inside the sandbox, with no password of its own: the
# only route in is the gateway's proxy, which already checks the session and
# that the caller owns this workspace. On Docker the port is published to
# 127.0.0.1 only; on Kubernetes it is never published at all.
CMD ["code-server", "--bind-addr", "0.0.0.0:8080", "--auth", "none", "--disable-telemetry", "/workspace"]
