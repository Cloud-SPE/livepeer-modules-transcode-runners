# Modules v2 compatibility and provenance

The reviewed implementation source is
`livepeer-modules-transcode` at Git revision
`18eaafab04b8f70a8e155497bd7bfb3bc1869240`.
This standalone port takes Go implementations, Go tests, contract fixtures,
live smoke scripts and embedded presets from these exact source paths:

- `transcode-core/`
- `transcode-runner/`
- `abr-runner/`
- `live-runner/`

Imports are adapted to this repository's single root module,
`github.com/Cloud-SPE/livepeer-modules-transcode-runners`. The standalone
`buildinfo.go` is retained for image build metadata. Source repository Go
modules, Makefiles, Dockerfiles, web apps and historical plans are not imported.
Root Docker infrastructure is adapted for the new live executable, pinned
MediaMTX, persistent state and explicit hardware policy. Direct-runner smoke
tooling is updated locally to consume terminal SSE.

The contract target is LOC `0eabff5` and network modules `08f5985` as reviewed
in the companion gateway migration. Runners do not import LOC or Modules:
compatibility is defined by runner self-description, paid protocols, request
schemas, descriptor/grants, callbacks and usage claims. Final production
compatibility also requires successful attachment and certification against
the actual deployed broker/catalog and GPU hardware.

## Breaking runtime changes

Batch jobs use terminal `paid-job/v1` exchanges and durable workload IDs.
ABR accepts `video-transcode-abr/v2`; VOD accepts `video-transcode-vod/v2`.
The runner reports measured delivered `video-frame-megapixel` in the private
`X-Livepeer-Work-Units` trailer. The old asynchronous status routes are gone.

Live uses broker create/status/terminate plus `rtmp-hls/v1` coordinates and
`stream-key-issue` grants. Private stream keys are issued separately.
MediaMTX owns routing and LL-HLS; runner callbacks meter finalized
`output_seconds`. The runner serves HLS at the descriptor URL. Gateway-created
MinIO URLs are not authoritative playback locations. Old FIFO-ingest and
S3-playlist upload assumptions are removed.

Existing v0 in-memory jobs/sessions cannot be recovered as v2 sessions.
Drain them before rollout. Retain new durable state and live encryption keys
across restarts. Deploy companion gateway/protocol changes together; do not
point a legacy gateway at these images.

## Validation boundary

Automated Docker Go tests, race checks, vet, build, compose configuration and
Node SSE smoke-parser tests establish source/runtime contract behavior. They
do not spend LOC credit or certify a production GPU. The operator must run
Modules certification, a real paid ABR job and live publish/HLS/refill/close
before admitting the new images to production traffic.

## Local acceptance evidence (2026-09-24)

All root Go tests with race detection, vet and package builds passed inside
Docker; all three Docker Go build stages and the tester image built. Compose
overlays and five Node SSE parser tests passed. A live runtime image built
through this standalone Dockerfile using the existing CPU FFmpeg validation
base passed readiness, metrics and graceful-stop smoke.

The local real-media scripts additionally passed VOD encoding of 60 frames
with one measured frame-megapixel, ABR audio-only delivery with zero video
units, terminal usage trailers, uploaded artifacts and durable replay checks.
Live passed real FFmpeg RTMP publication, master/rendition/finalized-segment
fetches, positive output-seconds metering, key rotation and old-key rejection,
resumed output, callbacks, and idempotent termination. No external paid API was
called. The vendor FFmpeg base matrix was not rebuilt or GPU-certified in this
validation; production acceptance remains Bead `runners-p1b`.
