# Infra workspace: OpenTofu, Terragrunt, TFLint, kubectl and Helm, plus the
# cloud CLIs' prerequisites. The provider plugin cache is under $HOME on the
# volume, so `tofu init` does not re-download providers after a restart.
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates openssh-client curl less zsh unzip gnupg jq python3 python3-pip \
 && rm -rf /var/lib/apt/lists/*

# All arch-aware: these run on amd64 and arm64 hosts alike.
RUN ARCH=$(dpkg --print-architecture) \
 && curl -fsSL https://get.opentofu.org/install-opentofu.sh | sh -s -- --install-method standalone --skip-verify \
 && curl -fsSL -o /usr/local/bin/terragrunt https://github.com/gruntwork-io/terragrunt/releases/latest/download/terragrunt_linux_${ARCH} \
 && chmod +x /usr/local/bin/terragrunt \
 && curl -fsSL https://raw.githubusercontent.com/terraform-linters/tflint/master/install_linux.sh | bash \
 && curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/$(curl -fsSL https://dl.k8s.io/release/stable.txt)/bin/linux/${ARCH}/kubectl" \
 && chmod +x /usr/local/bin/kubectl \
 && curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @earendil-works/pi-coding-agent
RUN curl -fsSL https://code-server.dev/install.sh | sh

RUN sed -i 's|^root:x:0:0:root:/root:.*|root:x:0:0:root:/workspace/.home:/usr/bin/zsh|' /etc/passwd
ENV HOME=/workspace/.home SHELL=/usr/bin/zsh
ENV TF_PLUGIN_CACHE_DIR=/workspace/.home/.terraform.d/plugin-cache
WORKDIR /workspace

# The plugin cache dir must exist before tofu will use it, and $HOME is created
# at first start on the volume, so make it on the way in.
CMD ["sh", "-c", "mkdir -p \"$TF_PLUGIN_CACHE_DIR\" && exec code-server --bind-addr 0.0.0.0:8080 --auth none --disable-telemetry /workspace"]
