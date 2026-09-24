#!/bin/sh
set -eu

smoke_image=${1:?image reference is required}
smoke_target=${2:?hardware target is required: nvidia, intel, or amd}
smoke_name="live-runner-hardware-smoke-$$"

cleanup() {
  docker rm --force "$smoke_name" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

case "$smoke_target" in
  nvidia)
    device_args="--gpus device=${NVIDIA_GPU_ID:-0}"
    encode_command='ffmpeg -hide_banner -loglevel error -f lavfi -i testsrc2=size=1280x720:rate=30 -t 3 -c:v h264_nvenc -f null -'
    ;;
  intel)
    device_args="--device ${DRI_DEVICE:-/dev/dri}:/dev/dri"
    encode_command='ffmpeg -hide_banner -loglevel error -f lavfi -i testsrc2=size=1280x720:rate=30 -t 3 -c:v h264_qsv -f null -'
    ;;
  amd)
    device_args="--device ${DRI_DEVICE:-/dev/dri}:/dev/dri"
    encode_command='ffmpeg -hide_banner -loglevel error -vaapi_device /dev/dri/renderD128 -f lavfi -i testsrc2=size=1280x720:rate=30 -t 3 -vf format=nv12,hwupload -c:v h264_vaapi -f null -'
    ;;
  *)
    echo "hardware smoke requires nvidia, intel, or amd" >&2
    exit 1
    ;;
esac

# The target encoder must initialize and consume frames on the real device;
# enumerating a compiled encoder is not certification.
# shellcheck disable=SC2086
docker run --rm $device_args --entrypoint sh "$smoke_image" -c "$encode_command"

# Startup exercises the runner's vendor and required-encoder admission check.
# shellcheck disable=SC2086
docker run --detach --name "$smoke_name" $device_args \
  --env LIVE_RUNNER_MASTER_KEY=bW1tbW1tbW1tbW1tbW1tbW1tbW1tbW1tbW1tbW1tbW0= \
  --env LIVE_RUNNER_BROKER_TOKEN=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  --env LIVE_RUNNER_INTERNAL_MEDIA_TOKEN=iiiiiiiiiiiiiiiiiiiiiiiiiiiiiiii \
  --env LIVEPEER_PUBLIC_RTMP_URL=rtmps://runner.invalid:1936 \
  --env LIVEPEER_PUBLIC_URL=https://runner.invalid/r/live-runner \
  "$smoke_image" >/dev/null

smoke_attempt=0
smoke_health=starting
while [ "$smoke_attempt" -lt 60 ]; do
  smoke_health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' "$smoke_name")
  if [ "$smoke_health" = healthy ]; then
    break
  fi
  if [ "$smoke_health" = unhealthy ]; then
    docker logs "$smoke_name" >&2
    exit 1
  fi
  smoke_attempt=$((smoke_attempt + 1))
  sleep 1
done
if [ "$smoke_health" != healthy ]; then
  docker logs "$smoke_name" >&2
  echo "live-runner image did not become healthy on $smoke_target hardware" >&2
  exit 1
fi

docker stop --timeout 10 "$smoke_name" >/dev/null
smoke_exit=$(docker inspect --format '{{.State.ExitCode}}' "$smoke_name")
if [ "$smoke_exit" -ne 0 ]; then
  echo "live-runner image exited with status $smoke_exit" >&2
  exit 1
fi

cleanup
trap - EXIT INT TERM
echo "$smoke_image initialized and encoded H.264 on real $smoke_target hardware"
