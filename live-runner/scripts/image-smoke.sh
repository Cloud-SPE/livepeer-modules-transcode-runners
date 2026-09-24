#!/bin/sh
set -eu

smoke_image=${1:?image reference is required}
smoke_target=${2:?hardware target is required: nvidia, intel, amd, or cpu}
smoke_name="live-runner-image-smoke-$$"

cleanup() {
  docker rm --force "$smoke_name" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

case "$smoke_target" in
  nvidia)
    required_encoders='h264_nvenc hevc_nvenc'
    ;;
  intel)
    required_encoders='h264_qsv hevc_qsv'
    docker run --rm --entrypoint sh "$smoke_image" -c 'command -v vainfo >/dev/null'
    ;;
  amd)
    required_encoders='h264_vaapi hevc_vaapi'
    docker run --rm --entrypoint sh "$smoke_image" -c 'command -v vainfo >/dev/null'
    ;;
  cpu)
    required_encoders='libx264'
    ;;
  *)
    echo "unsupported hardware target: $smoke_target" >&2
    exit 1
    ;;
esac

configured_target=$(docker image inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$smoke_image" | sed -n 's/^LIVE_RUNNER_HARDWARE=//p')
if [ "$configured_target" != "$smoke_target" ]; then
  echo "$smoke_image declares LIVE_RUNNER_HARDWARE=$configured_target, expected $smoke_target" >&2
  exit 1
fi

encoders=$(docker run --rm --entrypoint ffmpeg "$smoke_image" -hide_banner -encoders 2>/dev/null)
for encoder in $required_encoders; do
  printf '%s\n' "$encoders" | grep -F "$encoder" >/dev/null || {
    echo "$smoke_image does not contain $encoder" >&2
    exit 1
  }
done

docker run --detach --name "$smoke_name" \
  --env LIVE_RUNNER_HARDWARE=cpu \
  --env LIVE_RUNNER_MASTER_KEY=bW1tbW1tbW1tbW1tbW1tbW1tbW1tbW1tbW1tbW1tbW0= \
  --env LIVE_RUNNER_BROKER_TOKEN=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  --env LIVE_RUNNER_INTERNAL_MEDIA_TOKEN=iiiiiiiiiiiiiiiiiiiiiiiiiiiiiiii \
  --env LIVEPEER_PUBLIC_RTMP_URL=rtmps://runner.invalid:1936 \
  --env LIVEPEER_PUBLIC_URL=https://runner.invalid/r/live-runner \
  "$smoke_image" >/dev/null

smoke_attempt=0
while [ "$smoke_attempt" -lt 60 ]; do
  smoke_health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' "$smoke_name")
  if [ "$smoke_health" = healthy ]; then
    break
  fi
  if [ "$smoke_health" = unhealthy ]; then
    echo "live-runner image became unhealthy" >&2
    exit 1
  fi
  smoke_attempt=$((smoke_attempt + 1))
  sleep 1
done
if [ "$smoke_health" != healthy ]; then
  echo "live-runner image did not become healthy" >&2
  exit 1
fi

runner_metrics=$(docker exec "$smoke_name" curl --fail --silent http://127.0.0.1:9090/metrics)
printf '%s\n' "$runner_metrics" | grep -F 'live_ladder_starts_total' >/dev/null || {
  echo "live-runner metrics surface is missing ladder counters" >&2
  exit 1
}
printf '%s\n' "$runner_metrics" | grep -F 'live_callback_rejected_total' >/dev/null || {
  echo "live-runner metrics surface is missing callback counters" >&2
  exit 1
}

docker stop --timeout 10 "$smoke_name" >/dev/null
smoke_exit=$(docker inspect --format '{{.State.ExitCode}}' "$smoke_name")
if [ "$smoke_exit" -ne 0 ]; then
  echo "live-runner image exited with status $smoke_exit" >&2
  exit 1
fi

cleanup
trap - EXIT INT TERM

echo "$smoke_image ($smoke_target) image contract and CPU lifecycle verified"
