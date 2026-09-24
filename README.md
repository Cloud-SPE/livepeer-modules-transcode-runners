# livepeer-modules-transcode-runners

Standalone Go runners for Livepeer Modules v2. The broker authenticates and
accounts for paid work; these runners own FFmpeg execution and measured usage.
The companion Go video gateway uses LOC for funding and the Modules broker for
`paid-job/v1` and `paid-session/v1` dispatch.

All project work is tracked in Beads. Read [AGENTS.md](AGENTS.md) and
[WORKFLOW.md](WORKFLOW.md) before changing code.

| Runner | Contract | HTTP entry point | Unit |
|---|---|---|---|
| `transcode-runner` | `video-transcode-vod/v2`, terminal SSE | `POST /v1/video/transcode` | `video-frame-megapixel` |
| `abr-runner` | `video-transcode-abr/v2`, terminal SSE | `POST /v1/video/transcode/abr` | `video-frame-megapixel` |
| `live-runner` | `paid-session/v1`, `rtmp-hls/v1` descriptor | `POST /v1/sessions` | `output_seconds` |

Each runner advertises its authoritative attach contract at
`GET /.well-known/livepeer-runner`. The old asynchronous batch polling and
`/v1/video/live/sessions` endpoints are removed. Deploy with a v2 gateway and
broker together; this is a breaking upgrade.

## Build and validate

```sh
./build-images.sh test
./build-images.sh validate
TAG=v2-local ./build-images.sh build
```

Builds use one root Go module and root Docker contexts. NVIDIA, Intel and AMD
retain separate FFmpeg base images. NVIDIA defaults to CUDA 12.8.1 for Pascal
support. The live binary is `live-runner/cmd/live-runner`; its image also
includes pinned MediaMTX 1.20.1. See [BUILD.md](BUILD.md).

## Run

Copy `infra/env/nvidia-prod.env.example` to an ignored local env file and set
stable live secrets and public origins before starting:

```sh
docker compose --env-file .env.nvidia-prod -f infra/compose/docker-compose.nvidia-prod.yml up -d
```

Other GPUs use `infra/compose/docker-compose.runners.yml` with `--profile
intel`, `amd`, or `nvidia`. Only select one vendor on the same host ports.
Persist all state volumes. Attach the runners using current Modules templates;
runner ports and broker control routes belong on the operator network.

Live playback is served through the runner's advertised HLS URL. MediaMTX's
API and HLS listeners remain private. The gateway obtains a stream key through
the returned grant and relays RTMP to the advertised ingest URL. The
`live-standard` output profile meters finalized media on `720p`.

Read [API.md](API.md), [OPERATIONS.md](OPERATIONS.md), and
[TESTING.md](TESTING.md). Exact migration provenance and compatibility boundaries
are recorded in [MODULES-V2.md](MODULES-V2.md).
