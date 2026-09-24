# Runner API

`GET /.well-known/livepeer-runner` is the authoritative Modules attachment
contract for every runner. Attach using current Modules templates, not the
removed legacy profile fallback.

## Batch jobs

VOD accepts `POST /v1/video/transcode` with `video-transcode-vod/v2`.
ABR accepts `POST /v1/video/transcode/abr` with `video-transcode-abr/v2`.
Send `Content-Type: application/json` and `Accept: text/event-stream`.
Requests contain a stable `workload_id`, nested input URL, output artifact
references and upload URLs; ABR also selects a ladder preset. ABR destination
keys must match the selected preset exactly. See versioned fixtures under
`abr-runner/testdata/contracts/v2/` and VOD types in
`transcode-runner/vod_v2.go`.

One HTTP response streams progress and exactly one terminal `result` or `error`.
There is no 202/status-poll API. `X-Livepeer-Work-Units` is a runner-to-broker
HTTP trailer; the broker exposes the normative `Livepeer-Work-Units` claim.
Usage is `ceil(sum(actual_frames * width * height) / 1_000_000)` for delivered
video, including partial delivery on failure. Input duration is not settlement
evidence. Duplicate identical requests replay durable state; a changed request
with the same workload ID returns 409.

`GET /healthz` and each invocation route's `/presets` suffix remain available.

## Live sessions

| Method | Path | Authentication |
|---|---|---|
| POST | `/v1/sessions` | Member-agent tunnel, or configured broker bearer |
| GET, DELETE | `/v1/sessions/{id}` | Member-agent tunnel, or configured broker bearer |
| POST | `/v1/sessions/{id}/stream-keys` | Returned `stream-key-issue` grant bearer |
| GET | `/v1/public/sessions/{id}/status` | Public safe status |
| GET | `/ready` | Readiness |

The broker owns the session ID and includes `rtmp-hls-session/v1` parameters:
`publisher_mode`, `output_profile`, `metering_rendition`, and storage. The runner
returns `rtmp-hls/v1` public runtime coordinates and private grants. The gateway
uses the grant to obtain a stream key with an idempotent `request_id`. Keys
are not supplied in the create request. Fixtures are under
`live-runner/testdata/contracts/v1/`.

Usage is cumulative whole finalized `output_seconds` on the named metering
rendition, not wall time or a sum across the ladder. Callback events have
persistent IDs and sequences, use the per-session callback bearer, and retry
from a durable outbox. Closing a session stops ingest and playback and erases
its credentials once callback delivery is resolved.
