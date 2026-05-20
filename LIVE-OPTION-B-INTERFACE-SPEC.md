# LIVE OPTION B INTERFACE SPEC

Concrete interface spec for the Option B live topology:

`client -> transcode-gateway -> broker/payment -> live runner`

This document is written for three teams:

- `livepeer-modules-transcode-runners`
- `livepeer-network-modules`
- `livepeer-modules-transcode-gateway`

## Purpose

Define the exact contract boundaries required for:

- gateway -> broker live session control
- broker -> runner live session execution
- runner -> broker usage and state reporting

## Architectural rules

- The **gateway** is the customer-facing API surface.
- The **broker** is the session and payment authority.
- The **runner** is the media runtime authority.
- The **runner never talks to payment-daemon directly**.
- The **broker never needs customer identity inside the runner**.
- The **gateway never calls the live runner directly for paid control operations**.

## Canonical identifiers

Every component must persist these IDs explicitly.

| Field | Owner | Meaning |
|---|---|---|
| `gateway_session_id` | gateway | Customer-facing live session ID returned by `/v1/live`. |
| `broker_session_id` | broker | Broker-owned runtime/payment session ID. |
| `runner_session_id` | runner | Runner-owned media runtime session ID. |
| `work_id` | broker/payment | Payment session key used for debit/close. |

## Session state model

Allowed normalized states:

- `provisioning`
- `ready`
- `publishing`
- `ending`
- `ended`
- `failed`

Notes:

- Gateway may collapse `ready` and `publishing` into product-facing `live` if desired.
- Broker and runner should preserve the more detailed state internally.

## 1. Gateway -> Broker contract

The gateway opens and manages live sessions with the broker.

### 1.1 Open live session

`POST /v1/cap`

Headers:

- `Content-Type: application/json`
- `Livepeer-Capability: <live capability id>`
- `Livepeer-Offering: <offering id>`
- `Livepeer-Mode: live-session-remote-runner@v0`
- `Livepeer-Spec-Version: 0.1`
- `Livepeer-Request-Id: <uuid>`
- `Livepeer-Payment: <base64 envelope>`

Request body:

```json
{
  "gateway_session_id": "6d8f4a4d-09d7-4c1d-8d3e-c7d60c6114c4",
  "session_params": {
    "name": "launch-stream",
    "ladder": {
      "rungs": [
        { "name": "source", "passthrough": true },
        { "name": "720p", "width": 1280, "height": 720, "bitrate_kbps": 2500 },
        { "name": "480p", "width": 854, "height": 480, "bitrate_kbps": 1000 },
        { "name": "240p", "width": 426, "height": 240, "bitrate_kbps": 400 }
      ]
    },
    "idle_timeout_seconds": 30
  }
}
```

Response:

```json
{
  "gateway_session_id": "6d8f4a4d-09d7-4c1d-8d3e-c7d60c6114c4",
  "broker_session_id": "bsess_01jv6f6w0rpk6n6k7e2f1v9r9a",
  "runner_session_id": "rsess_01jv6f6w3z2q8gw1dvj9mpm6zb",
  "work_id": "3f8a1dd7-4cf1-4f4b-b7b3-bbdf819e63b4",
  "state": "ready",
  "media": {
    "ingest": {
      "rtmp_url": "rtmp://ingest.example.com/live",
      "stream_key": "lvk_4mY6fB7T2qR8kP1sW3dX9nL5cH0zA"
    },
    "playback": {
      "hls_url": "https://playback.example.com/live/rsess_01jv6f6w3z2q8gw1dvj9mpm6zb/master.m3u8"
    }
  },
  "control": {
    "topup_url": "https://broker.example.com/v1/cap/bsess_01jv6f6w0rpk6n6k7e2f1v9r9a/topup",
    "status_url": "https://broker.example.com/v1/cap/bsess_01jv6f6w0rpk6n6k7e2f1v9r9a",
    "end_url": "https://broker.example.com/v1/cap/bsess_01jv6f6w0rpk6n6k7e2f1v9r9a/end"
  },
  "expires_at": "2026-05-20T20:10:00Z"
}
```

Rules:

- Payment is required.
- Broker creates `broker_session_id` and `work_id`.
- Broker creates/binds the remote runner session before returning success.
- `stream_key` is returned only on this response.
- Gateway stores `broker_session_id`, `runner_session_id`, and `work_id`.

### 1.2 Top up live session

`POST /v1/cap/{broker_session_id}/topup`

Headers:

- `Content-Type: application/json`
- `Livepeer-Request-Id: <uuid>`
- `Livepeer-Payment: <base64 envelope>`

Request body:

```json
{
  "gateway_session_id": "6d8f4a4d-09d7-4c1d-8d3e-c7d60c6114c4"
}
```

Response:

```json
{
  "broker_session_id": "bsess_01jv6f6w0rpk6n6k7e2f1v9r9a",
  "work_id": "3f8a1dd7-4cf1-4f4b-b7b3-bbdf819e63b4",
  "state": "publishing",
  "balance": {
    "status": "ok",
    "runway_seconds_estimate": 184
  }
}
```

Rules:

- Payment is required.
- Top-up must credit the existing broker/payment session, not create a new one.

### 1.3 Query live session

`GET /v1/cap/{broker_session_id}`

Response:

```json
{
  "gateway_session_id": "6d8f4a4d-09d7-4c1d-8d3e-c7d60c6114c4",
  "broker_session_id": "bsess_01jv6f6w0rpk6n6k7e2f1v9r9a",
  "runner_session_id": "rsess_01jv6f6w3z2q8gw1dvj9mpm6zb",
  "work_id": "3f8a1dd7-4cf1-4f4b-b7b3-bbdf819e63b4",
  "state": "publishing",
  "media": {
    "ingest": {
      "rtmp_url": "rtmp://ingest.example.com/live"
    },
    "playback": {
      "hls_url": "https://playback.example.com/live/rsess_01jv6f6w3z2q8gw1dvj9mpm6zb/master.m3u8"
    }
  },
  "started_at": "2026-05-20T19:12:09Z",
  "last_heartbeat_at": "2026-05-20T19:13:01Z",
  "ended_at": null,
  "close_reason": null
}
```

Rules:

- No plaintext stream key after session creation.
- Broker may serve this from stored session state without contacting the runner synchronously.

### 1.4 End live session

`POST /v1/cap/{broker_session_id}/end`

Request body:

```json
{
  "reason": "gateway_close"
}
```

Response:

```json
{
  "broker_session_id": "bsess_01jv6f6w0rpk6n6k7e2f1v9r9a",
  "runner_session_id": "rsess_01jv6f6w3z2q8gw1dvj9mpm6zb",
  "state": "ended",
  "close_reason": "gateway_close",
  "ended_at": "2026-05-20T19:14:23Z"
}
```

Rules:

- This is idempotent.
- Broker must terminate the runner session and close payment state.

## 2. Broker -> Runner contract

The broker treats the live runner as a remote session backend.

Broker authentication to runner should use a runner-scoped shared secret or mTLS.

### 2.1 Create runner session

`POST /v1/video/live/sessions`

Headers:

- `Content-Type: application/json`
- `Authorization: Bearer <broker-runner-token>`

Request body:

```json
{
  "broker_session_id": "bsess_01jv6f6w0rpk6n6k7e2f1v9r9a",
  "work_id": "3f8a1dd7-4cf1-4f4b-b7b3-bbdf819e63b4",
  "capability_id": "livepeer:transcode/live-rtmp-hls-abr",
  "offering_id": "default",
  "session_params": {
    "name": "launch-stream",
    "ladder": {
      "rungs": [
        { "name": "source", "passthrough": true },
        { "name": "720p", "width": 1280, "height": 720, "bitrate_kbps": 2500 },
        { "name": "480p", "width": 854, "height": 480, "bitrate_kbps": 1000 },
        { "name": "240p", "width": 426, "height": 240, "bitrate_kbps": 400 }
      ]
    },
    "idle_timeout_seconds": 30
  },
  "broker_callbacks": {
    "event_url": "https://broker.example.com/internal/v1/live/events",
    "auth_token": "cb_01jv6f7h1y3n3ng8v9f0zw9vhe"
  }
}
```

Response:

```json
{
  "runner_session_id": "rsess_01jv6f6w3z2q8gw1dvj9mpm6zb",
  "state": "ready",
  "media": {
    "ingest": {
      "rtmp_url": "rtmp://ingest.example.com/live",
      "stream_key": "lvk_4mY6fB7T2qR8kP1sW3dX9nL5cH0zA"
    },
    "playback": {
      "hls_url": "https://playback.example.com/live/rsess_01jv6f6w3z2q8gw1dvj9mpm6zb/master.m3u8"
    }
  },
  "created_at": "2026-05-20T19:11:57Z"
}
```

Rules:

- Runner allocates `runner_session_id`.
- Runner owns media-plane coordinates.
- Runner must not know customer identity.

### 2.2 Query runner session

`GET /v1/video/live/sessions/{runner_session_id}`

Response:

```json
{
  "runner_session_id": "rsess_01jv6f6w3z2q8gw1dvj9mpm6zb",
  "broker_session_id": "bsess_01jv6f6w0rpk6n6k7e2f1v9r9a",
  "state": "publishing",
  "started_at": "2026-05-20T19:12:09Z",
  "last_packet_at": "2026-05-20T19:13:01Z",
  "last_heartbeat_at": "2026-05-20T19:13:01Z",
  "close_reason": null
}
```

### 2.3 Terminate runner session

`DELETE /v1/video/live/sessions/{runner_session_id}`

Request body:

```json
{
  "reason": "insufficient_balance"
}
```

Response:

```json
{
  "runner_session_id": "rsess_01jv6f6w3z2q8gw1dvj9mpm6zb",
  "state": "ended",
  "close_reason": "insufficient_balance",
  "ended_at": "2026-05-20T19:14:23Z"
}
```

Rules:

- Idempotent.
- Runner must tear down media runtime and stop usage emission.

## 3. Runner -> Broker event contract

Runner reports usage and state to the broker.

### 3.1 Event endpoint

`POST /internal/v1/live/events`

Headers:

- `Content-Type: application/json`
- `Authorization: Bearer <callback token>`

### 3.2 Event envelope

```json
{
  "broker_session_id": "bsess_01jv6f6w0rpk6n6k7e2f1v9r9a",
  "runner_session_id": "rsess_01jv6f6w3z2q8gw1dvj9mpm6zb",
  "event_id": "evt_01jv6f9q2tk7yhm6m63y8j5b6v",
  "sequence": 17,
  "event_type": "session.usage.tick",
  "event_time": "2026-05-20T19:13:00Z",
  "state": "publishing",
  "usage": {
    "unit": "output_seconds",
    "delta": 5,
    "total": 60
  },
  "close_reason": null,
  "details": {}
}
```

Rules:

- `sequence` must be monotonic per runner session.
- `event_id` must be unique for idempotency.
- `usage.total` is preferred; broker can derive deltas safely.

### 3.3 Required event types

#### `session.started`

```json
{
  "broker_session_id": "bsess_01jv6f6w0rpk6n6k7e2f1v9r9a",
  "runner_session_id": "rsess_01jv6f6w3z2q8gw1dvj9mpm6zb",
  "event_id": "evt_started_01",
  "sequence": 1,
  "event_type": "session.started",
  "event_time": "2026-05-20T19:12:09Z",
  "state": "publishing",
  "usage": {
    "unit": "output_seconds",
    "delta": 0,
    "total": 0
  },
  "close_reason": null,
  "details": {}
}
```

#### `session.heartbeat`

```json
{
  "broker_session_id": "bsess_01jv6f6w0rpk6n6k7e2f1v9r9a",
  "runner_session_id": "rsess_01jv6f6w3z2q8gw1dvj9mpm6zb",
  "event_id": "evt_hb_17",
  "sequence": 17,
  "event_type": "session.heartbeat",
  "event_time": "2026-05-20T19:13:00Z",
  "state": "publishing",
  "usage": {
    "unit": "output_seconds",
    "delta": 0,
    "total": 60
  },
  "close_reason": null,
  "details": {}
}
```

#### `session.usage.tick`

```json
{
  "broker_session_id": "bsess_01jv6f6w0rpk6n6k7e2f1v9r9a",
  "runner_session_id": "rsess_01jv6f6w3z2q8gw1dvj9mpm6zb",
  "event_id": "evt_tick_18",
  "sequence": 18,
  "event_type": "session.usage.tick",
  "event_time": "2026-05-20T19:13:05Z",
  "state": "publishing",
  "usage": {
    "unit": "output_seconds",
    "delta": 5,
    "total": 65
  },
  "close_reason": null,
  "details": {}
}
```

#### `session.failed`

```json
{
  "broker_session_id": "bsess_01jv6f6w0rpk6n6k7e2f1v9r9a",
  "runner_session_id": "rsess_01jv6f6w3z2q8gw1dvj9mpm6zb",
  "event_id": "evt_failed_22",
  "sequence": 22,
  "event_type": "session.failed",
  "event_time": "2026-05-20T19:14:02Z",
  "state": "failed",
  "usage": {
    "unit": "output_seconds",
    "delta": 0,
    "total": 81
  },
  "close_reason": "ffmpeg_exit_nonzero",
  "details": {
    "error_code": "FFMPEG_EXIT",
    "error_text": "ffmpeg exited with status 1"
  }
}
```

#### `session.ended`

```json
{
  "broker_session_id": "bsess_01jv6f6w0rpk6n6k7e2f1v9r9a",
  "runner_session_id": "rsess_01jv6f6w3z2q8gw1dvj9mpm6zb",
  "event_id": "evt_end_23",
  "sequence": 23,
  "event_type": "session.ended",
  "event_time": "2026-05-20T19:14:23Z",
  "state": "ended",
  "usage": {
    "unit": "output_seconds",
    "delta": 0,
    "total": 84
  },
  "close_reason": "gateway_close",
  "details": {}
}
```

## 4. Payment semantics

Payment ownership remains unchanged even though the media plane is remote.

### Broker responsibilities

- validate initial payment
- open receiver-side payment session
- accept top-ups
- derive debit deltas from runner usage events
- call `DebitBalance`
- call `SufficientBalance`
- terminate runner session on insufficient balance
- close payment session on terminal state

### Gateway responsibilities

- mint initial payment envelope
- mint top-up envelopes
- persist customer-facing commercial session state

### Runner responsibilities

- measure media work
- emit usage totals
- stop when broker requests termination

## 5. Error and close reasons

Allowed close reasons:

- `gateway_close`
- `customer_disconnect`
- `insufficient_balance`
- `idle_timeout`
- `ffmpeg_exit_nonzero`
- `runner_internal_error`
- `broker_shutdown`
- `runner_shutdown`

## 6. Repo-specific change instructions

### Team: `livepeer-network-modules`

Implement:

- a broker session-open contract matching section 1
- a remote live runner backend transport matching section 2
- a runner event intake endpoint matching section 3
- payment/debit/top-up behavior matching section 4

Do not ship:

- broker-local FFmpeg assumptions for Option B offerings
- payment-daemon coupling inside the runner transport

### Team: `livepeer-modules-transcode-gateway`

Implement:

- live open/top-up/query/end calls against the broker contract in section 1
- persistence of `broker_session_id`, `runner_session_id`, `work_id`
- mapping broker state to current `/v1/live` surface

Replace:

- old `/v1/session` broker integration assumptions

### Team: `livepeer-modules-transcode-runners`

Implement:

- `live-runner/` endpoints matching section 2
- event emission matching section 3
- FFmpeg/HLS media runtime consistent with section 4

Do not implement:

- direct payer-daemon integration
- customer API auth/billing logic

## 7. Suggested delivery order

1. Broker team finalizes sections 1, 3, and payment behavior.
2. Runner team implements section 2 and section 3 against a mocked broker.
3. Gateway team switches `/v1/live` to section 1.
4. Run one end-to-end test matrix:
   - open
   - publish
   - heartbeat
   - usage tick
   - top-up
   - close
   - insufficient balance
   - runner crash
