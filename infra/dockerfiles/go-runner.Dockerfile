ARG BASE_IMAGE=localbuild/ffmpeg-base-amd:v2-local
ARG MEDIAMTX_IMAGE=bluenviron/mediamtx:1.20.1@sha256:1b029d11049be75630e9b73bb0d5f47b08a7db4eaee89a80bf8f53bc40e56414
ARG GO_VERSION=1.25.7
ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=no-vcs
ARG BUILD_TIME=unknown
ARG RUNNER_DIR=transcode-runner
ARG BINARY_NAME=transcode-runner
ARG PRESET_NAME=transcode.yaml

FROM ${MEDIAMTX_IMAGE} AS mediamtx

FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY transcode-core/ ./transcode-core/
COPY transcode-runner/ ./transcode-runner/
COPY abr-runner/ ./abr-runner/
COPY live-runner/ ./live-runner/

ARG RUNNER_DIR
ARG RUNNER_PACKAGE=${RUNNER_DIR}
ARG BINARY_NAME
ARG BUILD_VERSION
ARG BUILD_COMMIT
ARG BUILD_TIME
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w \
      -X github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core.BuildVersion=${BUILD_VERSION} \
      -X github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core.BuildCommit=${BUILD_COMMIT} \
      -X github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core.BuildTime=${BUILD_TIME}" \
    -o "/bin/${BINARY_NAME}" "./${RUNNER_PACKAGE}"

FROM ${BASE_IMAGE}

ARG RUNNER_DIR
ARG BINARY_NAME
ARG PRESET_NAME

RUN apt-get update && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd -r runner \
    && useradd -r -g runner -d /home/runner -s /usr/sbin/nologin runner \
    && (getent group render >/dev/null || groupadd -r render) \
    && usermod -aG video,render runner \
    && mkdir -p /etc/runner/presets /tmp/transcode /tmp/abr /tmp/live /var/lib/transcode-runner /var/lib/abr-runner /var/lib/live-runner \
    && chown -R runner:runner /etc/runner /tmp/transcode /tmp/abr /tmp/live /var/lib/transcode-runner /var/lib/abr-runner /var/lib/live-runner

COPY --from=mediamtx /mediamtx /usr/local/bin/mediamtx
COPY --from=build "/bin/${BINARY_NAME}" "/usr/local/bin/${BINARY_NAME}"
COPY "${RUNNER_DIR}/presets.yaml" "/etc/runner/presets/${PRESET_NAME}"

ARG HARDWARE=auto
ENV LIVE_RUNNER_HARDWARE=${HARDWARE}
ENV LIVE_RUNNER_PRESETS_FILE=/etc/runner/presets/live.yaml
ENV RUNNER_BIN="/usr/local/bin/${BINARY_NAME}"

USER runner
EXPOSE 8080 1935
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD curl --fail --silent http://127.0.0.1:8080/ready || curl --fail --silent http://127.0.0.1:8080/healthz
ENTRYPOINT ["/bin/sh", "-lc", "exec \"$RUNNER_BIN\""]
