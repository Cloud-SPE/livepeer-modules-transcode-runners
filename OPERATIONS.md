# Operations

Deploy a v2-compatible gateway, LOC and Modules broker alongside these runners.
Runner self-description replaces legacy static runner profiles. Use current
Modules `video-transcode-vod`, `video-transcode-abr` and `video-transcode-live`
templates; offerings commonly use `vod-default`, `abr-default` and
`gateway-ingest`. Offering names remain discovery/operator policy.

Batch requires `STATE_DIR` on persistent storage. Do not delete journals while
work may be retried: that can repeat encoding and uploads. Signed URLs must
remain valid for execution and the recovery window; changing them changes the
canonical workload hash. The old `TEMP_DIR` and `JOB_TTL_SECONDS` environment
variables do not configure v2 recovery.

Live requires stable `LIVE_RUNNER_MASTER_KEY` (base64 32 bytes),
`LIVE_RUNNER_INTERNAL_MEDIA_TOKEN` (at least 32 characters),
`LIVE_RUNNER_PRESETS_FILE`, `LIVEPEER_PUBLIC_URL`, and
`LIVEPEER_PUBLIC_RTMP_URL`. Leave `LIVE_RUNNER_BROKER_TOKEN` empty for Modules member-agent tunnel
attachment: the tunnel authenticates the broker and does not forward a runner
bearer. For standalone control clients outside that tunnel, set an independent
strong token and require those clients to send it. Protect the state volume and master
key together; changing the key makes existing encrypted sessions unreadable.
Generate secrets with `openssl rand -base64 32` and store them outside git.

The public HTTP origin must route descriptor-advertised HLS, status and key
issuance paths to this runner. The RTMP origin must route to its MediaMTX
listener (or an RTMPS edge). Do not expose private MediaMTX API, HLS or metrics
listeners. Live readiness is `/ready`; batch readiness is `/healthz`.

The default live profile is `live-standard`, metered on `720p`. Output stalls
at 20 seconds and fails after 60 seconds by default. Configuration, durable
callback retries and hardware admission are validated at startup. `STATE_DIR`
and `LIVE_RUNNER_STATE_DIR` volumes in compose persist across container restarts.

For a GPU shared by live and batch, mount one writable host directory into all
three containers and set identical `GPU_ADMISSION_LOCK=/shared/encoder` paths.
Set local concurrency conservatively; actual GPU certification must encode
media and verify advancing HLS, not just list available encoders.

The two old `LIVE-OPTION-B-*` documents describe the retired v0 API and are
historical only. Current source types, fixtures, and [API.md](API.md) govern v2.
