# TESTING

## Unit tests

The shared `transcode-core` package contains the existing FFmpeg, preset, HLS,
I/O, and GPU helper tests.

Run inside a Go container or through future CI wiring; host Go is not required.

## Smoke tests

`transcode-tester/` is the direct-runner Node harness.

Primary flows:

- preset listing
- submit + poll single-rendition transcode
- submit + poll ABR transcode

Additional local `live-runner` coverage currently lives in Go unit tests:

- session create/get/delete API
- auth gate on the broker-facing API
- HLS static serving
- session store and watchdog behavior

The direct-runner mode is intentional for this repo: no proxy or broker shim is
required for local validation.
