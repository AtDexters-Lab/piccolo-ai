# Dependabot updates only the first FROM in a Dockerfile; keep the
# security-critical runtime there and derive the final image from this stage.
FROM openvino/model_server:2026.2.1-gpu@sha256:cc13f88dc249cb94ab30cab90286b0c72a329e3d605da497bd4e594c5d21af00 AS ovms

FROM golang:1.26.5-bookworm@sha256:1ecb7edf62a0408027bd5729dfd6b1b8766e578e8df93995b225dfd0944eb651 AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -buildid= -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/piccolo-ai-ovms ./cmd/piccolo-ai-ovms

FROM ovms

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="Piccolo AI OVMS Provider" \
      org.opencontainers.image.description="OpenAI-compatible local inference provider for Piccolo OS" \
      org.opencontainers.image.source="https://github.com/AtDexters-Lab/piccolo-ai" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

USER root
COPY --from=build --chown=5000:5000 /out/piccolo-ai-ovms /usr/local/bin/piccolo-ai-ovms
COPY --chown=5000:5000 LICENSE THIRD_PARTY_NOTICES.md /licenses/piccolo-ai/
RUN mkdir -p /var/cache/ovms && chown 5000:5000 /var/cache/ovms

USER ovms
EXPOSE 8000 8001
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=5s --start-period=120s --retries=3 \
  CMD ["/usr/local/bin/piccolo-ai-ovms", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/piccolo-ai-ovms"]
