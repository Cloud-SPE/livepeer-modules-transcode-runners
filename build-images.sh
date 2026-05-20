#!/usr/bin/env bash
# Single orchestrator for transcode runner images.
#
# Subcommands:
#   build [name ...]   Build all images, or a named subset.
#   push  [name ...]   Push deployable runner images to ${REGISTRY}.
#   validate           Run `docker compose config` against every overlay in infra/compose/.
#   clean              Remove locally-built images for ${REGISTRY}/${TAG}.
#   help               Show this help.
#
# Environment:
#   REGISTRY        Docker registry prefix for deployable runner images (default: tztcloud)
#   INTERNAL_REGISTRY  Local/internal prefix for build-only base images (default: localbuild)
#   TAG             Image tag (default: v1.3.1)
#   CUDA_VERSION    NVIDIA CUDA tag (default: 13.2.1)
#   UBUNTU_VERSION  Ubuntu version (default: 24.04)
#   GO_VERSION      Go toolchain (default: 1.25.7)
#   NODE_VERSION    Node major version for tester (default: 22)

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

REGISTRY="${REGISTRY:-tztcloud}"
INTERNAL_REGISTRY="${INTERNAL_REGISTRY:-localbuild}"
TAG="${TAG:-v1.3.1}"
CUDA_VERSION="${CUDA_VERSION:-13.2.1}"
UBUNTU_VERSION="${UBUNTU_VERSION:-24.04}"
GO_VERSION="${GO_VERSION:-1.25.7}"
NODE_VERSION="${NODE_VERSION:-22}"
BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
GIT_COMMIT="${GIT_COMMIT:-$(git rev-parse --short=12 HEAD 2>/dev/null || echo no-vcs)}"

ALL_IMAGES=(
  codecs-builder
  ffmpeg-base-nvidia
  ffmpeg-base-intel
  ffmpeg-base-amd
  transcode-runner-nvidia
  transcode-runner-intel
  transcode-runner-amd
  abr-runner-nvidia
  abr-runner-intel
  abr-runner-amd
  live-runner-nvidia
  live-runner-intel
  live-runner-amd
  transcode-tester
)

DEPLOYABLE_IMAGES=(
  transcode-runner-nvidia
  transcode-runner-intel
  transcode-runner-amd
  abr-runner-nvidia
  abr-runner-intel
  abr-runner-amd
  live-runner-nvidia
  live-runner-intel
  live-runner-amd
  transcode-tester
)

is_deployable_image() {
  local name="$1"
  for candidate in "${DEPLOYABLE_IMAGES[@]}"; do
    if [ "$candidate" = "$name" ]; then
      return 0
    fi
  done
  return 1
}

deploy_tag() { echo "${REGISTRY}/$1:${TAG}"; }
internal_tag() { echo "${INTERNAL_REGISTRY}/$1:${TAG}"; }

display_tag() {
  local name="$1"
  if is_deployable_image "$name"; then
    deploy_tag "$name"
  else
    internal_tag "$name"
  fi
}

build_codecs_builder() {
  docker build \
    --build-arg "UBUNTU_VERSION=${UBUNTU_VERSION}" \
    -t "$(internal_tag codecs-builder)" \
    -f infra/dockerfiles/codecs-builder.Dockerfile \
    .
}

build_ffmpeg_base() {
  local name="$1"
  docker build \
    --build-arg "REGISTRY=${INTERNAL_REGISTRY}" \
    --build-arg "TAG=${TAG}" \
    --build-arg "CUDA_VERSION=${CUDA_VERSION}" \
    --build-arg "UBUNTU_VERSION=${UBUNTU_VERSION}" \
    -t "$(internal_tag "${name}")" \
    -f "infra/dockerfiles/${name}.Dockerfile" \
    .
}

build_runner() {
  local image_name="$1"
  local base_name="$2"
  local runner_dir="$3"
  local bin_name="$4"
  local preset_name="$5"
  docker build \
    --build-arg "REGISTRY=${REGISTRY}" \
    --build-arg "TAG=${TAG}" \
    --build-arg "GO_VERSION=${GO_VERSION}" \
    --build-arg "BUILD_VERSION=${TAG}" \
    --build-arg "BUILD_COMMIT=${GIT_COMMIT}" \
    --build-arg "BUILD_TIME=${BUILD_TIME}" \
    --build-arg "BASE_IMAGE=$(internal_tag "${base_name}")" \
    --build-arg "RUNNER_DIR=${runner_dir}" \
    --build-arg "BINARY_NAME=${bin_name}" \
    --build-arg "PRESET_NAME=${preset_name}" \
    -t "$(deploy_tag "${image_name}")" \
    -f infra/dockerfiles/go-runner.Dockerfile \
    .
}

build_tester() {
  docker build \
    --build-arg "NODE_VERSION=${NODE_VERSION}" \
    -t "$(deploy_tag transcode-tester)" \
    -f infra/dockerfiles/transcode-tester.Dockerfile \
    .
}

build_one() {
  case "$1" in
    codecs-builder) build_codecs_builder ;;
    ffmpeg-base-nvidia) build_ffmpeg_base ffmpeg-base-nvidia ;;
    ffmpeg-base-intel) build_ffmpeg_base ffmpeg-base-intel ;;
    ffmpeg-base-amd) build_ffmpeg_base ffmpeg-base-amd ;;
    transcode-runner-nvidia) build_runner transcode-runner-nvidia ffmpeg-base-nvidia transcode-runner transcode-runner transcode.yaml ;;
    transcode-runner-intel) build_runner transcode-runner-intel ffmpeg-base-intel transcode-runner transcode-runner transcode.yaml ;;
    transcode-runner-amd) build_runner transcode-runner-amd ffmpeg-base-amd transcode-runner transcode-runner transcode.yaml ;;
    abr-runner-nvidia) build_runner abr-runner-nvidia ffmpeg-base-nvidia abr-runner abr-runner abr.yaml ;;
    abr-runner-intel) build_runner abr-runner-intel ffmpeg-base-intel abr-runner abr-runner abr.yaml ;;
    abr-runner-amd) build_runner abr-runner-amd ffmpeg-base-amd abr-runner abr-runner abr.yaml ;;
    live-runner-nvidia) build_runner live-runner-nvidia ffmpeg-base-nvidia live-runner live-runner live.yaml ;;
    live-runner-intel) build_runner live-runner-intel ffmpeg-base-intel live-runner live-runner live.yaml ;;
    live-runner-amd) build_runner live-runner-amd ffmpeg-base-amd live-runner live-runner live.yaml ;;
    transcode-tester) build_tester ;;
    *) echo "unknown image: $1" >&2; exit 2 ;;
  esac
}

cmd_build() {
  local targets=("$@")
  if [ "${#targets[@]}" -eq 0 ]; then
    targets=("${ALL_IMAGES[@]}")
  fi
  for name in "${targets[@]}"; do
    echo "==> Building $(display_tag "${name}")"
    echo "    version=${TAG} commit=${GIT_COMMIT} built=${BUILD_TIME}"
    build_one "${name}"
  done
}

cmd_push() {
  local targets=("$@")
  if [ "${#targets[@]}" -eq 0 ]; then
    targets=("${DEPLOYABLE_IMAGES[@]}")
  fi
  for name in "${targets[@]}"; do
    if ! is_deployable_image "$name"; then
      echo "refusing to push build-only image: $name" >&2
      exit 2
    fi
    echo "==> Pushing $(deploy_tag "${name}")"
    docker push "$(deploy_tag "${name}")"
  done
}

cmd_validate() {
  echo "==> Validating compose snippets in infra/compose/"
  shopt -s nullglob
  local snippets=(infra/compose/docker-compose.*.yml)
  if [ "${#snippets[@]}" -eq 0 ]; then
    echo "no compose snippets found under infra/compose/" >&2
    exit 1
  fi
  for f in "${snippets[@]}"; do
    echo "  - $f"
    docker compose -f "$f" config >/dev/null
  done
}

cmd_clean() {
  for name in "${ALL_IMAGES[@]}"; do
    docker rmi "$(display_tag "${name}")" 2>/dev/null || true
  done
}

cmd_help() {
  sed -n '2,/^set -euo pipefail/p' "$0" | sed 's/^# \{0,1\}//' | head -n -2
}

cmd="${1:-help}"
shift || true

case "${cmd}" in
  build) cmd_build "$@" ;;
  push) cmd_push "$@" ;;
  validate) cmd_validate ;;
  clean) cmd_clean ;;
  help|-h|--help) cmd_help ;;
  *) echo "unknown subcommand: ${cmd}" >&2; cmd_help; exit 2 ;;
esac
