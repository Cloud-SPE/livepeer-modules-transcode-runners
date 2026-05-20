# livepeer-modules-transcode-runners

Standalone home for the Livepeer video transcode runners. This repo ships three
Go HTTP runners plus shared FFmpeg/transcode logic, vendor-specific runtime
images for NVIDIA, Intel, and AMD, and direct-runner smoke tooling.

> **For agents:** start at [`AGENTS.md`](./AGENTS.md).

## What this repo ships

| Image | Purpose | Default endpoint |
|---|---|---|
| `transcode-runner-nvidia` / `-intel` / `-amd` | Single-rendition VOD transcode | `POST /v1/video/transcode` |
| `abr-runner-nvidia` / `-intel` / `-amd` | Multi-rendition ABR ladder transcode | `POST /v1/video/transcode/abr` |
| `live-runner-nvidia` / `-intel` / `-amd` | Live session runner for remote RTMP or gateway-ingest modes | `POST /v1/video/live/sessions` |
| `transcode-tester` | Node integration smoke harness | n/a |

Shared code lives in [`transcode-core/`](./transcode-core). Build and runtime
infrastructure lives in [`infra/`](./infra/).

## Build

Every gesture is Docker-first.

```bash
./build-images.sh build
./build-images.sh build transcode-runner-nvidia abr-runner-nvidia live-runner-nvidia
./build-images.sh validate
./build-images.sh clean
```

No host Go or host Node required.

## Compose

For vendor-generic local bring-up, use:

```bash
docker compose -f infra/compose/docker-compose.runners.yml --profile nvidia up -d
```

For a production-oriented NVIDIA node, use:

```bash
cp infra/env/nvidia-prod.env.example .env.nvidia-prod
docker compose --env-file .env.nvidia-prod -f infra/compose/docker-compose.nvidia-prod.yml up -d
```

That production stack defaults to the GTX 1080 tuned preset pack in
[`infra/presets/nvidia-gtx1080-transcode.yaml`](./infra/presets/nvidia-gtx1080-transcode.yaml)
and
[`infra/presets/nvidia-gtx1080-abr.yaml`](./infra/presets/nvidia-gtx1080-abr.yaml).
The live runner uses [`infra/presets/live.yaml`](./infra/presets/live.yaml) by
default.

## Live runner mode

`live-runner` uses a single gateway-ingest shape:

- shared RTMP ingest on one port
- direct FFmpeg ingest through a local FIFO
- HLS upload to caller-supplied S3-compatible storage

The broker must include `output_credential` and `ingest_accept.stream_key` in
the session-open request.

## Repo layout

```text
.
├── AGENTS.md, CLAUDE.md
├── README.md, DESIGN.md, BUILD.md, API.md
├── OPERATIONS.md, TESTING.md, SECURITY.md
├── build-images.sh
├── go.mod, go.sum
├── abr-runner/                 # ABR ladder runner source + embedded defaults
├── live-runner/                # remote live session runtime
├── transcode-runner/           # single-rendition runner source + embedded defaults
├── transcode-core/             # shared FFmpeg / GPU / preset / HLS logic
├── transcode-tester/           # Node smoke harness
├── infra/
│   ├── compose/                # docker-compose overlays
│   ├── dockerfiles/            # all Dockerfiles; build context is repo root
│   ├── env/                    # .env.example templates
│   ├── offerings/              # runner offering manifests
│   └── presets/                # operator-editable preset YAMLs
└── docs/
    ├── exec-plans/
    └── references/
```

## GPU support

All three vendor families are in scope:

- NVIDIA NVENC/NVDEC via CUDA 13 runtime images
- Intel QSV / VAAPI
- AMD VAAPI

Strict hardware mode is the default policy across vendors:

- jobs fail closed when the requested path would require CPU-only processing
- startup filters presets against the actual usable hardware/runtime path
- visible GPU hardware is not enough; the runner requires a working decode+encode runtime path

Current NVIDIA build note:

- CUDA `13.2.1` is supported
- FFmpeg is built without `libnpp` on this CUDA line due upstream API incompatibility with FFmpeg `7.1.3`

## What is intentionally absent

- No broker, gateway, or payment-layer code
- No checked-in secrets, keystores, or operator-local state
- No copied historical plan/doc tree from the source monorepo
