---
plan: 0001
title: Live Option B remote runner
status: active
phase: design
opened: 2026-05-20
owner: harness
related:
  - "../../..//LIVE-OPTION-B-COORDINATION.md"
  - "../../..//LIVE-OPTION-B-INTERFACE-SPEC.md"
  - "../../../../livepeer-modules-transcode-gateway"
  - "../../../../livepeer-network-modules"
---

# Plan 0001 — Live Option B remote runner

## 1. Goal

Add a first-class remote `live-runner/` to this repo for the Option B live
topology:

`client -> transcode-gateway -> broker/payment -> live runner`

In this topology:

- the gateway owns the customer API
- the broker owns session authority, routing, and payment enforcement
- the live runner owns the media runtime

## 2. Why this plan exists

This repo currently ships:

- `transcode-runner/` for single-rendition VOD
- `abr-runner/` for ABR VOD ladder work
- `transcode-core/` shared FFmpeg/GPU/HLS logic

It explicitly does not ship a live RTMP runner. Option B changes that.

The new work is not "extend ABR a little." It is a new class of workload:

- ABR is one request -> one job -> one completion
- live Option B is one open -> long-running session -> many usage ticks -> close

## 3. Local scope in this repo

This repo must add:

1. `live-runner/` service
2. live session manager
3. RTMP ingest session auth
4. FFmpeg live ladder supervision
5. HLS output serving or publishing surface
6. broker callback client for state and usage events
7. Docker/build/compose/docs/test support

This repo must not add:

- customer auth
- billing logic
- direct `payment-daemon` integration
- route selection logic

## 4. External dependencies

This repo cannot ship end-to-end Option B alone.

### 4.1 `livepeer-network-modules` must change

Required changes:

- broker support for a remote live runner backend transport
- broker session-open/top-up/query/end contract per
  `LIVE-OPTION-B-INTERFACE-SPEC.md`
- broker event intake for runner usage/state
- broker-side debit/runway enforcement based on runner events
- broker-side termination of runner session on insufficient balance
- resolver/coordinator/pool/config support for remote live offerings

Blocked local work if missing:

- true end-to-end integration
- top-up flow
- insufficient-balance termination
- authoritative session reconciliation

### 4.2 `livepeer-modules-transcode-gateway` must change

Required changes:

- replace the old live broker adapter contract
- switch `/v1/live` internals to the new broker session contract
- persist `broker_session_id`, `runner_session_id`, and `work_id`
- add live status reconciliation beyond create/delete
- support top-up and terminal-state propagation

Blocked local work if missing:

- final gateway integration
- customer-visible `/v1/live` correctness
- full session lifecycle testing from gateway entrypoint

## 5. Local architecture

### 5.1 New component

Add:

- `live-runner/` — RTMP live session runtime

Suggested responsibilities:

- `POST /v1/video/live/sessions`
- `GET /v1/video/live/sessions/{runner_session_id}`
- `DELETE /v1/video/live/sessions/{runner_session_id}`

### 5.2 Internal responsibilities

`live-runner/` should contain:

- HTTP API layer
- session store/state machine
- RTMP ingest listener
- FFmpeg supervisor
- HLS output manager
- broker event/callback client
- metrics and health endpoints

### 5.3 Shared-core responsibilities

`transcode-core/` should provide reusable primitives where sensible:

- GPU/runtime probing
- encoder selection
- FFmpeg live arg assembly
- HLS helper logic

The runner-local package should own orchestration and long-lived state rather
than pushing session authority into `transcode-core/`.

## 6. Session state model

Normalized runner states:

- `provisioning`
- `ready`
- `publishing`
- `ending`
- `ended`
- `failed`

State transitions:

1. session created -> `ready`
2. first successful publish/auth -> `publishing`
3. broker stop or local failure -> `ending`
4. cleanup complete -> `ended` or `failed`

## 7. Local implementation phases

## Phase 1 — API scaffold

Deliverables:

- `live-runner/` server entrypoint
- create/get/delete endpoints
- in-memory session store
- `runner_session_id` generation
- request/response shapes matching interface spec

Acceptance:

- API tests pass without media runtime enabled

## Phase 2 — RTMP ingest

Deliverables:

- per-session ingest coordinates
- session-bound `stream_key`
- RTMP publish auth
- state transition from `ready` to `publishing`
- disconnect handling

Acceptance:

- valid publish accepted
- bad stream key rejected
- disconnect transitions session correctly

## Phase 3 — FFmpeg live runtime

Deliverables:

- per-session FFmpeg launch
- ladder configuration support
- HLS output generation
- scratch/output directory lifecycle
- process supervision and cleanup

Acceptance:

- sample RTMP input produces playable HLS output
- FFmpeg exit is surfaced as `failed`

## Phase 4 — Broker callbacks

Deliverables:

- authenticated event client
- `session.started`
- `session.heartbeat`
- `session.usage.tick`
- `session.failed`
- `session.ended`
- retry/backoff and idempotency support

Acceptance:

- mock broker receives monotonic, replay-safe events

## Phase 5 — Hardening

Deliverables:

- idle timeout watchdog
- no-publish timeout
- forced terminate path
- stable close reasons
- metrics
- health/readiness endpoints

Acceptance:

- stop/close paths are idempotent
- stale sessions are cleaned up

## Phase 6 — Docker/docs/tests

Deliverables:

- Dockerfile under `infra/dockerfiles/`
- build integration via `build-images.sh`
- compose/env examples
- root-doc updates
- smoke harness additions

Acceptance:

- image builds with existing Docker-first workflow
- direct-runner smoke path exists for live runner

## 8. File/package plan

Suggested new paths:

- `live-runner/main.go`
- `live-runner/session.go`
- `live-runner/api.go`
- `live-runner/rtmp.go`
- `live-runner/ffmpeg.go`
- `live-runner/hls.go`
- `live-runner/callbacks.go`
- `live-runner/metrics.go`
- `live-runner/*_test.go`

Potential shared-core additions:

- `transcode-core/live_hls.go`
- `transcode-core/live_presets.go`
- `transcode-core/live_progress.go`

This is a direction, not a lock. The invariant is separation between:

- reusable FFmpeg/media helpers in `transcode-core/`
- long-lived session orchestration in `live-runner/`

## 9. Testing strategy

Local tests needed in this repo:

- API contract tests
- session state machine tests
- stream-key auth tests
- FFmpeg supervision tests
- event emission tests
- cleanup/idempotency tests

Docker-first smoke tests needed:

- create session
- publish RTMP
- observe HLS
- receive usage events in mock broker
- terminate session

## 10. Risks

Primary risks:

- mixing payment concerns into runner code
- overfitting local runner API before broker contract is stable
- making `transcode-core/` stateful in the wrong places
- under-specifying usage units for broker debit logic
- weak cleanup semantics leaving orphan FFmpeg processes or session dirs

## 11. Exit criteria

This plan is complete when:

1. `live-runner/` exists and builds in Docker
2. runner exposes create/get/delete session API
3. RTMP ingest authenticates against per-session keys
4. FFmpeg live ladder execution produces HLS output
5. runner emits broker-compatible state and usage events
6. runner terminates cleanly on broker stop
7. docs and smoke coverage exist in this repo

## 12. Cross-repo coordination summary

Before local implementation is considered final:

1. `livepeer-network-modules` must implement the remote live broker contract
2. `livepeer-modules-transcode-gateway` must adopt the new broker live API
3. end-to-end create/publish/top-up/close/insufficient-balance flow must be validated across all three repos
