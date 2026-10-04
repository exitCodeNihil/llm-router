FROM --platform=$BUILDPLATFORM node:26-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json* ./
RUN npm install
COPY web/ .
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-s -w -X main.version=${VERSION}" -o /llmrouter ./cmd/llmrouter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /llmrouter /llmrouter
EXPOSE 8080
ENTRYPOINT ["/llmrouter"]
