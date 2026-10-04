# Python workspace. Node is present only to run the pi agent.
FROM python:3.13-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates openssh-client curl less zsh \
 && rm -rf /var/lib/apt/lists/*

# pi needs Node >= 22.19; NodeSource is the shortest path to it on slim.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @earendil-works/pi-coding-agent

RUN curl -fsSL https://code-server.dev/install.sh | sh

# OpenSSH expands "~" via getpwuid(), not $HOME — without this an uploaded ssh
# key is written to the volume and then never found. See base.Dockerfile.
RUN sed -i 's|^root:x:0:0:root:/root:.*|root:x:0:0:root:/workspace/.home:/usr/bin/zsh|' /etc/passwd
ENV HOME=/workspace/.home SHELL=/usr/bin/zsh
WORKDIR /workspace

CMD ["code-server", "--bind-addr", "0.0.0.0:8080", "--auth", "none", "--disable-telemetry", "/workspace"]
