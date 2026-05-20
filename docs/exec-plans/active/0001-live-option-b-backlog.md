---
plan: 0001-backlog
title: Live Option B local backlog
status: active
phase: execution
opened: 2026-05-20
owner: harness
related:
  - "./0001-live-option-b-remote-runner.md"
  - "../../../LIVE-OPTION-B-INTERFACE-SPEC.md"
---

# Plan 0001 backlog — Live Option B local backlog

PR-sized task list for `livepeer-modules-transcode-runners` only.

## 1. Backlog shape

Each item below should be small enough to land independently, or nearly so.

Dependency markers:

- `blocks`: must land first
- `parallel`: can proceed concurrently once prerequisites exist
- `external`: depends on another repo team's contract or implementation

## 2. Backlog

### B001 — Scaffold `live-runner/`

Scope:

- create `live-runner/`
- add server entrypoint
- add basic routing
- add health endpoint
- add config/env parsing

Files likely touched:

- `live-runner/main.go`
- `live-runner/config.go`
- `live-runner/api.go`

Acceptance:

- binary starts
- Docker build can target the new component later

Depends on:

- none

### B002 — Session model and in-memory store

Scope:

- define runner session record
- define normalized session states
- implement in-memory store
- generate `runner_session_id`

Files likely touched:

- `live-runner/session.go`
- `live-runner/store.go`
- `live-runner/session_test.go`

Acceptance:

- create/get/delete session primitives exist
- state transitions are unit-tested

Depends on:

- `B001`

### B003 — Create/get/delete HTTP API

Scope:

- implement:
  - `POST /v1/video/live/sessions`
  - `GET /v1/video/live/sessions/{runner_session_id}`
  - `DELETE /v1/video/live/sessions/{runner_session_id}`
- validate request/response payloads against the local spec

Files likely touched:

- `live-runner/api.go`
- `live-runner/handlers.go`
- `live-runner/api_test.go`

Acceptance:

- API tests pass with no RTMP/FFmpeg runtime yet

Depends on:

- `B002`

### B004 — Broker auth for runner control API

Scope:

- add broker-to-runner auth mechanism
- reject unauthenticated create/get/delete requests
- keep auth simple and explicit, likely bearer token first

Files likely touched:

- `live-runner/auth.go`
- `live-runner/middleware.go`
- `live-runner/auth_test.go`

Acceptance:

- control API requires broker auth

Depends on:

- `B003`

### B005 — RTMP ingest listener scaffold

Scope:

- add RTMP listener startup/shutdown
- bind listener lifecycle to runner process
- define per-session publish lookup hook

Files likely touched:

- `live-runner/rtmp.go`
- `live-runner/rtmp_test.go`

Acceptance:

- listener starts and stops cleanly
- session lookup wiring exists

Depends on:

- `B002`

### B006 — Stream key generation and publish auth

Scope:

- mint per-session `stream_key`
- return ingest coordinates on session create
- authenticate RTMP publish against session state
- reject invalid or duplicate publishes per defined policy

Files likely touched:

- `live-runner/session.go`
- `live-runner/rtmp.go`
- `live-runner/rtmp_auth_test.go`

Acceptance:

- valid publish accepted
- invalid key rejected
- duplicate publish behavior is deterministic

Depends on:

- `B005`

### B007 — HLS scratch/output manager

Scope:

- define per-session HLS output root
- create and clean up scratch/output paths
- decide local file serving shape for development and smoke

Files likely touched:

- `live-runner/hls.go`
- `live-runner/hls_test.go`

Acceptance:

- per-session output dirs created and cleaned up

Depends on:

- `B002`

### B008 — FFmpeg live command integration

Scope:

- wire `transcode-core` live command building into runner runtime
- identify missing shared-core helpers
- avoid embedding orchestration into `transcode-core`

Files likely touched:

- `live-runner/ffmpeg.go`
- `transcode-core/live.go`
- possibly new `transcode-core/live_*`

Acceptance:

- runner can build a valid FFmpeg live command for a session

Depends on:

- `B007`

### B009 — FFmpeg supervisor and session lifecycle wiring

Scope:

- spawn FFmpeg per publish/session
- transition state to `publishing`
- detect process exit
- mark terminal states and close reasons

Files likely touched:

- `live-runner/ffmpeg.go`
- `live-runner/session.go`
- `live-runner/ffmpeg_test.go`

Acceptance:

- FFmpeg exit updates session state
- cleanup path is deterministic

Depends on:

- `B008`

### B010 — Minimal HLS serving for smoke/dev

Scope:

- expose session playback files over HTTP
- return `hls_url` from session create
- keep serving shape simple for v0 local testing

Files likely touched:

- `live-runner/hls_http.go`
- `live-runner/api.go`
- `live-runner/hls_http_test.go`

Acceptance:

- created session returns playable HLS base URL once publishing begins

Depends on:

- `B007`

### B011 — Usage counter abstraction

Scope:

- define canonical live usage unit for runner output
- expose cumulative totals
- keep broker-facing usage independent of gateway/customer concepts

Files likely touched:

- `live-runner/usage.go`
- `live-runner/usage_test.go`

Acceptance:

- cumulative usage can be read safely while session is running

Depends on:

- `B009`

### B012 — Broker callback client scaffold

Scope:

- implement authenticated event POST client
- retries/backoff
- event envelope builder

Files likely touched:

- `live-runner/callbacks.go`
- `live-runner/callbacks_test.go`

Acceptance:

- mock broker can receive signed/authenticated events

Depends on:

- `B003`

### B013 — Emit `session.started` and `session.heartbeat`

Scope:

- emit start event on first good publish/runtime start
- emit periodic heartbeats while session runs

Files likely touched:

- `live-runner/callbacks.go`
- `live-runner/session.go`
- `live-runner/heartbeat_test.go`

Acceptance:

- monotonic events emitted to mock broker

Depends on:

- `B012`
- `B009`

### B014 — Emit `session.usage.tick`

Scope:

- emit periodic usage events with cumulative totals
- define cadence config

Files likely touched:

- `live-runner/usage.go`
- `live-runner/callbacks.go`
- `live-runner/usage_tick_test.go`

Acceptance:

- broker-facing usage events are monotonic and replay-safe

Depends on:

- `B011`
- `B012`

### B015 — Emit terminal events

Scope:

- emit `session.failed`
- emit `session.ended`
- ensure one terminal event path per session

Files likely touched:

- `live-runner/session.go`
- `live-runner/callbacks.go`
- `live-runner/terminal_test.go`

Acceptance:

- failure and normal close produce the right terminal event once

Depends on:

- `B013`
- `B014`

### B016 — Broker-forced terminate path

Scope:

- make `DELETE /v1/video/live/sessions/{runner_session_id}` stop a live session promptly
- ensure FFmpeg, RTMP state, and HLS cleanup follow

Files likely touched:

- `live-runner/handlers.go`
- `live-runner/session.go`
- `live-runner/terminate_test.go`

Acceptance:

- forced stop is idempotent
- running session shuts down cleanly

Depends on:

- `B009`

### B017 — Timeouts and watchdogs

Scope:

- no-publish timeout
- idle/stall timeout
- watchdog cleanup

Files likely touched:

- `live-runner/watchdog.go`
- `live-runner/watchdog_test.go`

Acceptance:

- stale sessions close with stable reasons

Depends on:

- `B006`
- `B009`

### B018 — Metrics

Scope:

- active sessions
- publish accepts/rejects
- FFmpeg failures
- event emit attempts
- usage totals

Files likely touched:

- `live-runner/metrics.go`
- `live-runner/metrics_test.go`

Acceptance:

- metrics surface exists and increments on core flows

Depends on:

- `B009`

### B019 — Docker image and build wiring

Scope:

- add Dockerfile in `infra/dockerfiles/`
- wire `build-images.sh`
- keep non-root runtime and repo build conventions

Files likely touched:

- `infra/dockerfiles/live-runner*.Dockerfile`
- `build-images.sh`
- compose/env templates as needed

Acceptance:

- `./build-images.sh build` includes live runner image target

Depends on:

- `B003`

### B020 — Compose/env examples

Scope:

- add local dev compose example
- add env templates
- include broker callback target examples

Files likely touched:

- `infra/compose/*`
- `infra/env/*`

Acceptance:

- local smoke deployment path is documented and runnable

Depends on:

- `B019`

### B021 — Runner smoke harness

Scope:

- extend smoke tooling to cover live runner
- mock broker callback receiver
- publish test RTMP sample and verify HLS output

Files likely touched:

- `transcode-tester/` or new live smoke harness files

Acceptance:

- Docker-first smoke covers create/publish/hls/terminate

Depends on:

- `B010`
- `B015`
- `B019`

### B022 — Root docs update

Scope:

- update root docs for live runner presence and contract

Files likely touched:

- `README.md`
- `DESIGN.md`
- `BUILD.md`
- `API.md`
- `OPERATIONS.md`
- `TESTING.md`
- `SECURITY.md`

Acceptance:

- repo docs reflect shipped live runner design

Depends on:

- `B019`

## 3. External dependency backlog

These are not local tasks but must be tracked.

### E001 — Broker contract finalized

Repo:

- `livepeer-network-modules`

Needed for:

- final request/response compatibility
- event callback auth details
- top-up flow

Local tasks affected:

- `B003`
- `B012`
- `B014`
- `B015`

### E002 — Gateway live adapter migration

Repo:

- `livepeer-modules-transcode-gateway`

Needed for:

- end-to-end `/v1/live` compatibility

Local tasks affected:

- `B021`
- final validation after local implementation

## 4. Suggested PR batches

### Batch A — Runnable scaffold

- `B001`
- `B002`
- `B003`
- `B004`

### Batch B — Media plane

- `B005`
- `B006`
- `B007`
- `B008`
- `B009`
- `B010`

### Batch C — Broker integration

- `B011`
- `B012`
- `B013`
- `B014`
- `B015`
- `B016`

### Batch D — Hardening and shipment

- `B017`
- `B018`
- `B019`
- `B020`
- `B021`
- `B022`

## 5. Minimum viable local milestone

The first meaningful end-to-end local milestone is:

- create live session
- publish RTMP with session key
- produce HLS
- emit mock broker events
- terminate cleanly

That milestone corresponds roughly to:

- `B001` through `B016`

## 6. Recommended next task

Start with:

- `B001`
- `B002`
- `B003`

Those tasks create the stable local shape without waiting on the full media or broker integration details.
