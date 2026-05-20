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
  - creates a gateway-ingest live runner session
  - response includes `runner_session_id` and `private_ingest_url`
- `GET /v1/video/live/sessions/{runner_session_id}`
  - returns current runner session state, ingest/output health, and cumulative
    usage
- `DELETE /v1/video/live/sessions/{runner_session_id}`
  - terminates a live session
- `GET /healthz`

### Gateway-ingest mode request fields

`POST /v1/video/live/sessions` may include:

- `output_credential`
  - S3-compatible endpoint, bucket, prefix, and temporary credentials used for
    HLS upload
- `ingest_accept.stream_key`
  - stream key the shared RTMP ingress must accept

The runner requires these fields and:

- returns `private_ingest_url`
- uploads HLS playlists and segments to the provided object store
- does not serve playback from local HTTP

## Notes

- Status polling is `POST`, not `GET`
- Request bodies are JSON
- Upload and download URLs are caller-provided
- `live-runner` is a broker-facing service, not a customer-facing API
- Webhook callbacks remain supported by the source runner code
- `webhook_url` is caller-supplied per job request; the runner does not derive it from env
- in containerized deployments, `localhost` in `webhook_url` points at the runner container itself
