# Architecture

Three runners share `transcode-core`, a Go library for GPU detection, FFmpeg
commands, presets, HLS, media probing and GPU admission. They do not implement
LOC funding, customer identity, or payment validation.

Batch runners accept one versioned request with a stable `workload_id`, validate
its canonical digest, persist admission, and stream progress through a terminal
SSE event. Identical retries attach to the same durable execution/result;
changed content under the same ID fails. The broker extracts measured delivered
frame-megapixels from the terminal response trailer. Journal files survive
container replacement through dedicated state volumes.

The live runner receives broker session parameters and callback credentials,
creates an encrypted durable session, and returns a runtime descriptor plus a
scoped key-issue grant. MediaMTX authenticates publishers using runner-issued
keys. FFmpeg publishes a live ladder to private MediaMTX paths; the runner
proxies playable HLS and counts finalized segments on the metering rendition.
Durable sequenced callbacks communicate cumulative `output_seconds` to the
broker. Restart recovers state, key rotation, termination and pending callbacks.

All images run non-root. NVIDIA, Intel and AMD runtime bases remain separate;
live hardware admission fails closed for the image's declared vendor. On a
shared physical GPU, configure the same `GPU_ADMISSION_LOCK` mount in all
containers to exclude live and batch cohorts from each other.
