# The gateway as a static binary on distroless. The builder runs on the build
# machine's own architecture and cross-compiles with zig, so a multi-arch build
# never compiles Rust under emulation.
FROM --platform=$BUILDPLATFORM docker.io/library/rust:1.89-slim-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends python3-pip \
    && pip3 install --break-system-packages ziglang==0.17.0 \
    && cargo install --locked cargo-zigbuild@0.23.4 \
    && rustup target add x86_64-unknown-linux-musl aarch64-unknown-linux-musl
WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY src src
ARG TARGETARCH
ARG VERSION=dev
RUN target=$([ "$TARGETARCH" = arm64 ] && echo aarch64 || echo x86_64)-unknown-linux-musl \
    && LLMR_VERSION=$VERSION cargo zigbuild --release --locked --target "$target" \
    && cp "target/$target/release/llmr-gateway" /llmr-gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /llmr-gateway /llmr-gateway
EXPOSE 8080
ENTRYPOINT ["/llmr-gateway"]
