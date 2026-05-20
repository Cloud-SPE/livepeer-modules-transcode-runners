# DESIGN

This repo ships two HTTP runners that orchestrate FFmpeg subprocesses for VOD
video workloads:

- `transcode-runner` handles single-rendition transcode jobs
- `abr-runner` handles multi-rendition ABR ladder jobs

Both binaries import the shared `transcode-core` package, which owns:

- GPU detection and encoder selection
- FFmpeg and ffprobe command construction
- Preset parsing and validation
- HLS playlist generation
- Progress parsing
- Download/upload helpers
- Filter graph construction for subtitles, watermarking, thumbnails, and tone mapping

## Mental model

The broker or any compatible upstream submits a job over HTTP. The runner:

1. validates the request
2. downloads the input
3. probes media with `ffprobe`
4. runs FFmpeg with the best available hardware path
5. uploads outputs to caller-provided URLs
6. exposes job status over a polling endpoint

Job state is in-memory only. Restarts lose active and historical job state.

## Image strategy

The repo uses shared vendor-specific FFmpeg runtime bases:

- `ffmpeg-base-nvidia` on CUDA 13
- `ffmpeg-base-intel` on Ubuntu 24.04 + Intel media stack
- `ffmpeg-base-amd` on Ubuntu 24.04 + VAAPI stack

Each runner image then adds only:

- the statically linked Go binary
- the embedded preset file path override
- the temp dir
- a non-root runtime user

## Clean-slate constraints

- No legacy live runner
- No source-monorepo historical docs copied forward
- No secrets or operator-local state
- No broker-specific assumptions in the direct-runner smoke path
