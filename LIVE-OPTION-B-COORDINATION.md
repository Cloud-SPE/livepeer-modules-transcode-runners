# LIVE OPTION B COORDINATION

One-page coordination doc for the remote live runner topology:

`client -> transcode-gateway -> broker/payment -> live runner`

## Goal

Ship RTMP live where:

- the **gateway** owns the customer API
- the **broker** owns routing, payment, and session authority
- the **live runner** owns the media runtime

This is different from the current broker-local RTMP pipeline. In Option B, the
live runner becomes the execution backend for the media plane.

## Non-goals

- No direct customer billing logic in the runner
- No customer-identity awareness in the runner
- No broker bypass for paid session authority
- No change to ABR topology unless separately planned

## Canonical flow

1. Client calls `POST /v1/live` on `livepeer-modules-transcode-gateway`.
2. Gateway resolves a broker route and mints the initial `Livepeer-Payment`.
3. Gateway opens a live session with the broker.
4. Broker validates payment and creates a broker-owned live session.
5. Broker creates or binds a remote live session on the live runner.
6. Broker returns ingest + playback coordinates to the gateway.
7. Client publishes RTMP to the runner-owned media plane.
8. Runner emits usage/state to the broker during the session.
9. Broker drives `DebitBalance` / `SufficientBalance` and requests top-ups via the gateway when needed.
10. Gateway tops up the broker session as needed.
11. On close, failure, or insufficient balance, broker terminates the runner session and settles payment state.

## Ownership

### `livepeer-modules-transcode-runners`

Own:

- `live-runner/` service
- session lifecycle inside the media runtime
- RTMP ingest handling or ingest endpoint ownership
- FFmpeg live ladder execution
- HLS output
- runner status, heartbeats, terminal state
- usage measurement emitted to broker

Do not own:

- customer auth
- customer billing
- route selection
- receiver-side payment-daemon state

### `livepeer-network-modules`

Own:

- broker live session authority
- resolver/coordinator compatibility for remote live backends
- payment validation, debit, runway checks, top-up acceptance
- broker session state machine
- broker-to-runner live transport contract
- forced termination on insufficient balance or policy failure

### `livepeer-modules-transcode-gateway`

Own:

- `/v1/live` customer API
- gateway reservation and live session persistence
- route selection request to resolver
- payer-daemon sender integration
- broker session open / top-up / close calls
- customer-visible live status surface

## Cross-repo contract to lock first

These identifiers must be explicit and durable:

- `gateway_session_id`
- `broker_session_id`
- `runner_session_id`
- `work_id`

These APIs must be frozen before implementation begins.

### Gateway -> Broker

`POST /v1/cap`

- Purpose: open live session
- Payment: required
- Returns:
  - `broker_session_id`
  - `runner_session_id` if already allocated
  - ingest coordinates
  - playback coordinates
  - optional control/top-up metadata

`POST /v1/cap/{broker_session_id}/topup`

- Purpose: add funded runway to an existing live session
- Payment: required
- Returns:
  - current session status
  - accepted funding metadata

`GET /v1/cap/{broker_session_id}`

- Purpose: query broker session state
- Payment: not required
- Returns:
  - state
  - runner binding
  - playback info
  - terminal reason if ended

`POST /v1/cap/{broker_session_id}/end`

- Purpose: close the session
- Payment: not required
- Returns:
  - terminal status

### Broker -> Runner

`POST /v1/video/live/sessions`

- Purpose: create runner live session
- Body:
  - `broker_session_id`
  - ladder/preset config
  - media config
  - callback/control coordinates
- Returns:
  - `runner_session_id`
  - ingest coordinates
  - playback coordinates

`GET /v1/video/live/sessions/{runner_session_id}`

- Purpose: query runner state

`DELETE /v1/video/live/sessions/{runner_session_id}`

- Purpose: terminate runner session

### Runner -> Broker

At minimum the broker needs these events or equivalent polled state:

- `session.started`
- `session.usage.tick`
- `session.heartbeat`
- `session.failed`
- `session.ended`

Each event must carry:

- `broker_session_id`
- `runner_session_id`
- monotonic sequence or timestamp
- cumulative or delta work units
- terminal reason when applicable

## Recommended semantics

### Payment

- Runner never calls `payment-daemon` directly.
- Runner reports usage.
- Broker performs `DebitBalance` and `SufficientBalance`.
- Gateway performs initial funding and top-ups.

### Media plane

- Customer media should not traverse the broker when avoidable.
- Broker remains session authority even when runner owns media execution.
- Ingest/playback URLs may be broker-issued but should resolve to runner-owned runtime surfaces.

### Failure handling

- Broker is authoritative for insufficient-balance termination.
- Runner is authoritative for media-runtime failure detection.
- Gateway is authoritative for customer-visible API state.

## Repo checklists

### `livepeer-modules-transcode-runners`

- Add `live-runner/`
- Define session create/get/delete API
- Implement RTMP ingest and HLS output
- Reuse `transcode-core` FFmpeg/GPU helpers
- Add runner->broker event/callback client
- Add tests for start, publish, HLS availability, stop, failure, cleanup

### `livepeer-network-modules`

- Add broker support for remote live runner backend transport
- Replace broker-local FFmpeg assumption for Option B live offerings
- Add broker<->runner session binding and state persistence
- Add usage/top-up/termination flow for remote live sessions
- Update resolver/coordinator/pool validation for remote live topology
- Add conformance and lifecycle tests

### `livepeer-modules-transcode-gateway`

- Update live broker client to the new broker session contract
- Keep `/v1/live` product API stable where possible
- Store broker session id and runner session id if needed
- Add top-up and richer status handling
- Add migration(s) for extra live session metadata
- Add end-to-end tests against the new broker contract

## Recommended sequence

1. Lock the broker<->runner live contract.
2. Lock the gateway<->broker live contract.
3. Implement broker support for remote live sessions.
4. Implement `live-runner/` against that broker contract.
5. Update transcode gateway live adapter and persistence.
6. Run end-to-end validation: create, publish, top-up, close, insufficient-balance termination, runner crash.

## Decision summary

- **ABR stays as-is.** It remains request/response broker dispatch.
- **Live changes to Option B.** It becomes broker-authorized, runner-executed remote session work.
- **Runner stays blind to customer identity and billing.**
- **Broker stays the payment and session authority.**
