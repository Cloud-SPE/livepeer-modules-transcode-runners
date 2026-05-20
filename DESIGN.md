# DESIGN

This repo ships three HTTP runners that orchestrate FFmpeg subprocesses for VOD
and live video workloads:

- `transcode-runner` handles single-rendition transcode jobs
- `abr-runner` handles multi-rendition ABR ladder jobs
- `live-runner` handles live RTMP ingest and session-oriented HLS output work

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

For `live-runner`, the shape is session-oriented instead:

1. broker creates a runner session over HTTP
2. runner accepts gateway-owned RTMP on one shared ingest port
3. runner starts an FFmpeg live HLS runtime
4. publisher or gateway pushes RTMP into the runner ingest plane
5. runner emits heartbeat, publish, upload, and usage events back to the broker
6. broker closes the runner session over HTTP when the live session ends

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

`live-runner` keeps session state in memory and per-session scratch on local
disk. It uploads HLS artifacts to caller-supplied S3-compatible storage and
does not serve playback itself. It remains blind to customer identity and
billing state; the broker is still the payment and session authority.

## Clean-slate constraints

- No source-monorepo historical docs copied forward
- No secrets or operator-local state
- No broker-specific assumptions in the direct-runner smoke path
