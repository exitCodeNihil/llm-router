# Node workspace: node 22, npm/pnpm/yarn via corepack, TypeScript.
# The npm cache lives under $HOME (/workspace/.home/.npm), i.e. on the volume,
# so a second `npm install` in a fresh container is warm.
FROM node:22-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates openssh-client curl less zsh build-essential python3 \
 && rm -rf /var/lib/apt/lists/*

RUN corepack enable && npm install -g typescript @earendil-works/pi-coding-agent
RUN curl -fsSL https://code-server.dev/install.sh | sh

RUN sed -i 's|^root:x:0:0:root:/root:.*|root:x:0:0:root:/workspace/.home:/usr/bin/zsh|' /etc/passwd
ENV HOME=/workspace/.home SHELL=/usr/bin/zsh
ENV NPM_CONFIG_CACHE=/workspace/.home/.npm PNPM_HOME=/workspace/.home/.pnpm COREPACK_HOME=/workspace/.home/.corepack
WORKDIR /workspace

CMD ["code-server", "--bind-addr", "0.0.0.0:8080", "--auth", "none", "--disable-telemetry", "/workspace"]
