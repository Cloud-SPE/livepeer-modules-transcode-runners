# API

## transcode-runner

- `POST /v1/video/transcode`
  - submits a single-rendition job
  - returns `202` with `job_id`
- `POST /v1/video/transcode/status`
  - accepts `{ "job_id": "..." }`
  - returns current job status
- `GET /v1/video/transcode/presets`
  - lists active presets after hardware filtering
- `GET /healthz`

## abr-runner

- `POST /v1/video/transcode/abr`
  - submits an ABR job
  - returns `202` with `job_id` and selected renditions
- `POST /v1/video/transcode/abr/status`
  - accepts `{ "job_id": "..." }`
  - returns current job and per-rendition status
- `GET /v1/video/transcode/abr/presets`
  - lists active ABR presets after hardware filtering
- `GET /healthz`

## live-runner

- `POST /v1/video/live/sessions`
  - creates a remote live session
  - returns `201` with `runner_session_id`, `rtmp_url`, `stream_key`, and `hls_url`
- `GET /v1/video/live/sessions/{runner_session_id}`
  - returns current runner session state and cumulative usage
- `DELETE /v1/video/live/sessions/{runner_session_id}`
  - terminates a live session
- `GET /_hls/{runner_session_id}/master.m3u8`
  - serves the session master playlist when the session is publishing
- `GET /healthz`

## Notes

- Status polling is `POST`, not `GET`
- Request bodies are JSON
- Upload and download URLs are caller-provided
- `live-runner` is a broker-facing service, not a customer-facing API
- Webhook callbacks remain supported by the source runner code
- `webhook_url` is caller-supplied per job request; the runner does not derive it from env
- in containerized deployments, `localhost` in `webhook_url` points at the runner container itself
