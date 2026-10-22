# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26.0
ARG NODE_VERSION=24

FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-bookworm-slim AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web/ ./
# CI runs type and contract checks before the publish workflow starts.
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY --from=frontend /src/internal/webui/dist/ ./internal/webui/dist/
# Keep target and version changes from invalidating the shared dependency layer.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/pi-gateway ./cmd/pi-gateway

FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 gateway \
    && useradd --uid 10001 --gid gateway --no-create-home --shell /usr/sbin/nologin gateway \
    && mkdir -p /app/data && chown 10001:10001 /app/data
WORKDIR /app
COPY --from=backend /out/pi-gateway /usr/local/bin/pi-gateway
COPY config.example.yaml /app/config.yaml
ENV PI_GATEWAY_HOST=0.0.0.0 PI_GATEWAY_PORT=8317 PI_GATEWAY_DATABASE=/app/data/pi-gateway.db
USER 10001:10001
EXPOSE 8317
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["/usr/local/bin/pi-gateway", "--healthcheck", "--config", "/app/config.yaml"]
ENTRYPOINT ["/usr/local/bin/pi-gateway"]
CMD ["-config", "/app/config.yaml"]
