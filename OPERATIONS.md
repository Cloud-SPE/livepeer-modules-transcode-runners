# OPERATIONS

Runtime configuration templates live in [`infra/env/`](./infra/env/).

Compose overlays live in [`infra/compose/`](./infra/compose/).

## GPU modes

- NVIDIA: use the `nvidia` profile and a host with NVIDIA Container Toolkit
- Intel: use the `intel` profile and pass `/dev/dri`
- AMD: use the default `amd` profile and pass `/dev/dri`

For a long-running NVIDIA node, prefer [`docker-compose.nvidia-prod.yml`](./infra/compose/docker-compose.nvidia-prod.yml) plus [`nvidia-prod.env.example`](./infra/env/nvidia-prod.env.example).

NVIDIA health on the host matters before the runner does:

- `nvidia-smi` should work on the host
- Docker GPU injection should work for a trivial CUDA container before testing runner jobs
- if host driver and user-space libraries are mismatched, the NVIDIA runner image will still start, but strict GPU mode will mark the runtime unusable and disable all GPU-bound presets
- startup now logs the exact GPU detection failure reason, including runtime sanity-check failures

On a GTX 1080 specifically:

- H.264 NVENC should be available
- HEVC NVENC should be available
- AV1 encode is not available
- the production NVIDIA stack defaults to:
  - [`nvidia-gtx1080-transcode.yaml`](./infra/presets/nvidia-gtx1080-transcode.yaml)
  - [`nvidia-gtx1080-abr.yaml`](./infra/presets/nvidia-gtx1080-abr.yaml)
- 4K and AV1 are intentionally excluded from that pack
- `TRANSCODE_PRESETS_FILE` and `ABR_PRESETS_FILE` can be overridden in the env file if you want a different preset pack

## Presets

Operator-editable presets live in [`infra/presets/`](./infra/presets/).

Set:

- `PRESETS_FILE=/etc/runner/presets/transcode.yaml` for `transcode-runner`
- `PRESETS_FILE=/etc/runner/presets/abr.yaml` for `abr-runner`
- `PRESETS_FILE=/etc/runner/presets/live.yaml` for `live-runner`

If unset, each runner falls back to its embedded preset file.

## Strict GPU mode

- `GPU_STRICT` defaults to `true` on all vendors
- strict mode rejects request features that currently require CPU-side processing:
  - subtitle burn-in
  - watermark overlay
  - thumbnail extraction
- strict mode also rejects jobs when hardware decode for the input codec or hardware encode for the output codec is unavailable

If you need best-effort development behavior, you can explicitly set `GPU_STRICT=false`, but that is not the production default.

## State and storage

- Jobs are stored in memory only
- `live-runner` sessions are stored in memory only
- Scratch space lives under `/tmp/transcode`, `/tmp/abr`, or `/tmp/live`
- Output upload targets must be pre-signed or otherwise writable URLs

## Live runner

`live-runner` is a broker-facing remote session backend.

Key runtime knobs:

- `RUNNER_INGEST_PUBLIC_HOST` — host returned in gateway-ingest `private_ingest_url`
- `RUNNER_SHARED_INGEST_ADDR` — shared RTMP ingest bind address for gateway-ingest mode, default `:1935`
- `SESSION_NO_PUBLISH_TTL` — max wait for first RTMP publish
- `SESSION_IDLE_TTL` — max stall window after publishing starts
- `OUTPUT_SYNC_INTERVAL` — cadence for gateway-ingest S3 sync polling
- `OUTPUT_FAILURE_THRESHOLD` — consecutive uploader failures before a publishing session stalls
- `BROKER_AUTH_TOKEN` — optional bearer token required on the control API

Ingress topology is fixed:

- one shared RTMP ingest port, typically `1935`
- broker-facing HTTP control API on `RUNNER_ADDR`
- HLS output uploaded to the caller-supplied S3-compatible target

Operators should expose:

- the HTTP control port
- the shared RTMP ingest port
