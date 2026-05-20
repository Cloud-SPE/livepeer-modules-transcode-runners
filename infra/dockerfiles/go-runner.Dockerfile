ARG BASE_IMAGE=localbuild/ffmpeg-base-amd:v1.3.1
ARG GO_VERSION=1.25.7
ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=no-vcs
ARG BUILD_TIME=unknown
ARG RUNNER_DIR=transcode-runner
ARG BINARY_NAME=transcode-runner
ARG PRESET_NAME=transcode.yaml

FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY transcode-core/ ./transcode-core/
COPY transcode-runner/ ./transcode-runner/
COPY abr-runner/ ./abr-runner/
COPY live-runner/ ./live-runner/

ARG RUNNER_DIR
ARG BINARY_NAME
ARG BUILD_VERSION
ARG BUILD_COMMIT
ARG BUILD_TIME
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w \
      -X github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core.BuildVersion=${BUILD_VERSION} \
      -X github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core.BuildCommit=${BUILD_COMMIT} \
      -X github.com/Cloud-SPE/livepeer-modules-transcode-runners/transcode-core.BuildTime=${BUILD_TIME}" \
    -o "/bin/${BINARY_NAME}" "./${RUNNER_DIR}"

FROM ${BASE_IMAGE}

ARG RUNNER_DIR
ARG BINARY_NAME
ARG PRESET_NAME

RUN groupadd -r runner \
    && useradd -r -g runner -d /home/runner -s /usr/sbin/nologin runner \
    && groupadd -r render 2>/dev/null || true \
    && usermod -aG video,render runner \
    && mkdir -p /etc/runner/presets /tmp/transcode /tmp/abr /tmp/live \
    && chown -R runner:runner /etc/runner /tmp/transcode /tmp/abr /tmp/live

COPY --from=build "/bin/${BINARY_NAME}" "/usr/local/bin/${BINARY_NAME}"
COPY "${RUNNER_DIR}/presets.yaml" "/etc/runner/presets/${PRESET_NAME}"

ENV RUNNER_BIN="/usr/local/bin/${BINARY_NAME}"

USER runner
EXPOSE 8080
ENTRYPOINT ["/bin/sh", "-lc", "exec \"$RUNNER_BIN\""]
