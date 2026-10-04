# Go workspace: the current Go toolchain, golangci-lint, and delve for the IDE.
# GOPATH and the build cache are under $HOME on the volume, so module downloads
# and compiled packages survive a container recreate.
FROM golang:1.26-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates openssh-client curl less zsh \
 && rm -rf /var/lib/apt/lists/*

# pi is a Node CLI (>= 22.19); NodeSource is the shortest path on Debian.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @earendil-works/pi-coding-agent
RUN curl -fsSL https://code-server.dev/install.sh | sh

RUN curl -fsSL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b /usr/local/bin \
 && GOBIN=/usr/local/bin go install github.com/go-delve/delve/cmd/dlv@latest \
 && GOBIN=/usr/local/bin go install golang.org/x/tools/gopls@latest

RUN sed -i 's|^root:x:0:0:root:/root:.*|root:x:0:0:root:/workspace/.home:/usr/bin/zsh|' /etc/passwd
ENV HOME=/workspace/.home SHELL=/usr/bin/zsh
ENV GOPATH=/workspace/.home/go GOCACHE=/workspace/.home/.cache/go-build GOMODCACHE=/workspace/.home/go/pkg/mod
ENV PATH=$PATH:/workspace/.home/go/bin
WORKDIR /workspace

CMD ["code-server", "--bind-addr", "0.0.0.0:8080", "--auth", "none", "--disable-telemetry", "/workspace"]
