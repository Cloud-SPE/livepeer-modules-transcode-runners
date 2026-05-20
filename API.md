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

## Notes

- Status polling is `POST`, not `GET`
- Request bodies are JSON
- Upload and download URLs are caller-provided
- Webhook callbacks remain supported by the source runner code
