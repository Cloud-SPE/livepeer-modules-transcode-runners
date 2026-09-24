# Validation

```sh
./build-images.sh test
./build-images.sh validate
docker run --rm -v "$PWD/transcode-tester:/app:ro" -w /app node:22-alpine node --test sse.test.mjs
```

The Docker Go gate runs race-enabled tests, vet, and builds for the root module.
Tests cover strict contract decoding, terminal SSE claims, durable idempotency,
ABR recovery, measured frames, encrypted live state, stream keys, callbacks,
HLS routing, finalized-segment usage, process lifecycle and hardware policy.

The tester connects directly to runners. `presets` and `contract` inspect
read-only endpoints. `quick` requires `REQUEST_FILE` with a valid v2 request
and fresh presigned URLs; it consumes the stream and fails on a missing or
failed terminal event. Mount that request file into the tester container.
Never commit files containing usable signed URLs.

```sh
docker run --rm --network host -v /secure/request.json:/fixtures/request.json:ro \
  -e ABR_BASE_URL=http://localhost:8087 -e REQUEST_FILE=/fixtures/request.json \
  tztcloud/transcode-tester:v2-local node test-abr.mjs quick
```

Deployment acceptance additionally needs real NVIDIA/Intel/AMD encoding,
Modules runner attachment/certification, a paid LOC-funded ABR completion,
and live publish/playback/refill/close with broker-signed settlement evidence.
Source tests and image compilation cannot establish those deployment outcomes.
The live scripts under `live-runner/scripts/` verify runtime image and hardware
admission on a suitable host; full paid acceptance belongs with the gateway.

## Local real-media acceptance

`infra/scripts/local-live-smoke.py` and `local-batch-smoke.py` use Python 3
standard-library orchestration, Docker FFmpeg/runners and loopback-only test
services. They never contact LOC or spend funds. Run from the repo root. Set
`LIVE_SMOKE_IMAGE`, `VOD_SMOKE_IMAGE`, and `ABR_SMOKE_IMAGE` to the built image
references. The images need CPU FFmpeg for these tests; live explicitly selects
CPU, VOD mounts a CPU fixture and ABR uses the audio-only fixture without
weakening video GPU admission. Ports 18076–18079, 18088–18089, 11935, 18888,
19091 and 19997–19998 must be free.

```sh
python3 infra/scripts/local-batch-smoke.py
python3 infra/scripts/local-live-smoke.py
```

Batch verifies actual uploaded media, terminal SSE/trailers, delivered frame
counts, replay without duplicate uploads and conflicting workload rejection.
Live verifies actual RTMP publication, master/rendition/segment retrieval,
positive cumulative output seconds, stream-key rotation/revocation, resumed
output and idempotent termination with immediately unavailable playback.
These CPU tests do not certify vendor hardware or a paid broker path.
