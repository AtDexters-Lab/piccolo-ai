# syntax=docker/dockerfile:1.7

# These are BuildKit named contexts supplied by
# scripts/build-llama-openvino-image.sh. Keeping them external lets the exact
# validated build from an intentionally dirty llama.cpp worktree be packaged
# without pretending that its source state has already been committed.
FROM llama-src AS llama_source
FROM llama-build AS llama_build
FROM openvino-runtime AS openvino_runtime

FROM openvino/model_server:2026.2-gpu@sha256:fdc34725b35d79cde23cff1967f90efb40237b7cf5545a70460209056f3da733

USER root
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates \
        clinfo \
        curl \
        intel-gpu-tools \
        jq \
        libgomp1 \
        libssl3 \
        libtbb12 \
        nginx-light \
        openssh-server \
        pciutils \
        procps \
        supervisor \
        tini && \
    rm -rf /var/lib/apt/lists/* && \
    usermod --login developer --home /home/developer --move-home --shell /bin/bash ovms && \
    groupmod --new-name developer ovms && \
    { getent group render >/dev/null || groupadd --system render; } && \
    usermod --append --groups video,render developer && \
    passwd --delete developer && \
    install -d -o developer -g developer -m 0755 /home/developer

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG PICCOLO_AI_PATCH_SHA256=unknown
ARG LLAMA_REVISION=unknown
ARG LLAMA_PATCH_SHA256=unknown
ARG OPENVINO_VERSION=unknown

LABEL org.opencontainers.image.title="Piccolo AI Gemma 4 OpenVINO Bring-up Provider" \
      org.opencontainers.image.description="SSH-enabled llama.cpp/OpenVINO GPU bring-up provider for Piccolo OS" \
      org.opencontainers.image.source="https://github.com/AtDexters-Lab/piccolo-ai" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      com.atdexters.piccolo-ai.patch-sha256="${PICCOLO_AI_PATCH_SHA256}" \
      com.atdexters.piccolo-ai.llama-revision="${LLAMA_REVISION}" \
      com.atdexters.piccolo-ai.llama-patch-sha256="${LLAMA_PATCH_SHA256}" \
      com.atdexters.piccolo-ai.openvino-version="${OPENVINO_VERSION}"

# The build script constructs this named context from an explicit runtime
# allowlist and preserves its shared-library symlinks.
COPY --from=llama_build / /opt/llama/bin/
COPY --from=openvino_runtime / /opt/openvino/
COPY --from=llama_source /LICENSE /licenses/llama.cpp/LICENSE
COPY LICENSE THIRD_PARTY_NOTICES.md /licenses/piccolo-ai/

COPY backends/llama-openvino/nginx.conf /etc/piccolo-ai/nginx.conf
COPY backends/llama-openvino/proxy_params /etc/piccolo-ai/proxy_params
COPY backends/llama-openvino/sshd_config /etc/piccolo-ai/sshd_config
COPY backends/llama-openvino/supervisord.conf /etc/piccolo-ai/supervisord.conf
COPY backends/llama-openvino/entrypoint.sh /usr/local/bin/piccolo-ai-llama-entrypoint
COPY backends/llama-openvino/gemma-control.sh /usr/local/bin/gemma
COPY backends/llama-openvino/gemma-diagnose.sh /usr/local/bin/gemma-diagnose
COPY backends/llama-openvino/gemma-run.sh /usr/local/bin/gemma-run
COPY backends/llama-openvino/gemma-snapshot.sh /usr/local/bin/gemma-snapshot

RUN chmod 0755 \
        /usr/local/bin/piccolo-ai-llama-entrypoint \
        /usr/local/bin/gemma \
        /usr/local/bin/gemma-diagnose \
        /usr/local/bin/gemma-run \
        /usr/local/bin/gemma-snapshot && \
    ln -s /opt/llama/bin/llama-bench /usr/local/bin/llama-bench && \
    ln -s /opt/llama/bin/llama-mtmd-debug /usr/local/bin/llama-mtmd-debug && \
    ln -s /opt/llama/bin/llama-server /usr/local/bin/llama-server && \
    ln -s /opt/llama/bin/test-backend-ops /usr/local/bin/test-backend-ops && \
    printf '%s\n' \
        /opt/llama/bin \
        /opt/openvino/runtime/lib/intel64 \
        /opt/openvino/runtime/3rdparty/tbb/lib \
        > /etc/ld.so.conf.d/piccolo-ai.conf && \
    ldconfig && \
    printf '%s\n' \
        "piccolo-ai=${COMMIT}" \
        "piccolo-ai.patch.sha256=${PICCOLO_AI_PATCH_SHA256}" \
        "llama.cpp=${LLAMA_REVISION}" \
        "llama.patch.sha256=${LLAMA_PATCH_SHA256}" \
        "openvino=${OPENVINO_VERSION}" \
        > /etc/piccolo-ai/source-state

ENV PATH="/opt/llama/bin:${PATH}" \
    LD_LIBRARY_PATH="/opt/llama/bin:/opt/openvino/runtime/lib/intel64:/opt/openvino/runtime/3rdparty/tbb/lib:${LD_LIBRARY_PATH}" \
    OpenVINO_DIR="/opt/openvino/runtime/cmake" \
    GGML_OPENVINO_DEVICE="GPU" \
    GGML_OPENVINO_CACHE_DIR="/var/lib/piccolo-ai/openvino-cache" \
    GGML_OPENVINO_CACHE_MAX_ENTRIES="5" \
    GGML_OPENVINO_CACHE_LOG="1" \
    GGML_OPENVINO_SUPPORTS_LOG="1"

EXPOSE 22 8000 8001
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["curl", "--fail", "--silent", "--show-error", "http://127.0.0.1:8000/healthz"]
ENTRYPOINT ["/usr/local/bin/piccolo-ai-llama-entrypoint"]
