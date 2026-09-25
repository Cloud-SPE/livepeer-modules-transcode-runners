# Operations

Deploy a v2-compatible gateway, LOC and Modules broker alongside these runners.
Runner self-description replaces legacy static runner profiles. Use current
Modules `video-transcode-vod`, `video-transcode-abr` and `video-transcode-live`
templates; offerings commonly use `vod-default`, `abr-default` and
`gateway-ingest`. Offering names remain discovery/operator policy.

Batch requires `STATE_DIR` on persistent storage. Do not delete journals while
work may be retried: that can repeat encoding and uploads. Signed URLs must
remain valid for execution and the recovery window; changing them changes the
canonical workload hash. The old `TEMP_DIR` and `JOB_TTL_SECONDS` environment
variables do not configure v2 recovery.

Live requires stable `LIVE_RUNNER_MASTER_KEY` (base64 32 bytes),
`LIVE_RUNNER_INTERNAL_MEDIA_TOKEN` (at least 32 characters),
`LIVE_RUNNER_PRESETS_FILE`, `LIVEPEER_PUBLIC_URL`, and
`LIVEPEER_PUBLIC_RTMP_URL`. Leave `LIVE_RUNNER_BROKER_TOKEN` empty for Modules member-agent tunnel
attachment: the tunnel authenticates the broker and does not forward a runner
bearer. For standalone control clients outside that tunnel, set an independent
strong token and require those clients to send it. Protect the state volume and master
key together; changing the key makes existing encrypted sessions unreadable.
Generate secrets with `openssl rand -base64 32` and store them outside git.

The public HTTP origin must route descriptor-advertised HLS, status and key
issuance paths to this runner. The RTMP origin must route to its MediaMTX
listener (or an RTMPS edge). Do not expose private MediaMTX API, HLS or metrics
listeners. Live readiness is `/ready`; batch readiness is `/healthz`.

The default live profile is `live-standard`, metered on `720p`. Output stalls
at 20 seconds and fails after 60 seconds by default. Configuration, durable
callback retries and hardware admission are validated at startup. `STATE_DIR`
and `LIVE_RUNNER_STATE_DIR` volumes in compose persist across container restarts.

For a GPU shared by live and batch, mount one writable host directory into all
three containers and set identical `GPU_ADMISSION_LOCK=/shared/encoder` paths.
Set local concurrency conservatively; actual GPU certification must encode
media and verify advancing HLS, not just list available encoders.

The two old `LIVE-OPTION-B-*` documents describe the retired v0 API and are
historical only. Current source types, fixtures, and [API.md](API.md) govern v2.

## Publisher inactivity and browser playback

Implemented under `runners-2qp` and `runners-4v4`.

`LIVE_RUNNER_INITIAL_PUBLISH_TIMEOUT` defaults to `5m` and ends sessions
whose publisher never connects. `LIVE_RUNNER_RECONNECT_GRACE` defaults to
`2m`, starting at the first observed RTMP disconnect. Reconnecting cancels
that deadline. A deadline already persisted retains its value across restart
or configuration changes. Unknown router/API failures do not count as an
offline observation. Browser presence never controls runner lifetime.
Both timeouts end normally with the existing `publisher_disconnect` reason;
final usage is cumulative finalized output seconds, not waiting time. Terminal
callbacks use the existing durable retry queue. Connected publishers with no
output still use the independent output stall/failure deadlines.

Public HLS supports cross-origin GET/HEAD and Range preflight; management APIs
do not inherit this CORS policy. Upstream MediaMTX cookies are private and
isolated by rendition. Playback becomes unavailable when the session ends.

For an HTTPS portal, set `LIVEPEER_PUBLIC_URL=https://live.example.com` before
creating sessions. A TLS edge is required; setting the variable alone does not
serve TLS. Existing session descriptors retain their originally issued URLs.
Use an existing reverse proxy or the standalone Caddy configuration:

```sh
LIVE_HLS_HOST=live.example.com LIVE_HLS_UPSTREAM=host.docker.internal:8088 \
  docker compose -f infra/compose/docker-compose.live-https.yml up -d
```

Point that hostname's DNS to the runner host and make ports 80/443 reachable
for certificate issuance. If those ports already belong to an existing proxy,
add the HLS route there instead. Set `LIVE_HLS_UPSTREAM` to the actual private
runner HTTP endpoint (including its port); it must be reachable from the edge
container. Keep runner management and metrics private. The bundled edge
proxies HLS assets and preserves Range and LL-HLS query parameters. It also
forwards the grant-authenticated stream-key issuance and status URLs, because
`LIVEPEER_PUBLIC_URL` supplies those descriptor URLs as well. Without that
stream-key route, the gateway cannot start or renew its RTMP relay. The runner
continues enforcing grants; CORS applies only to HLS, not these runtime APIs. It does not
change RTMP ingress or attach the runner to a broker.

After deployment, create a fresh session, publish media, and verify HTTPS
master, both video/audio rendition playlists, and segments from the portal
origin. Verify disconnect/reconnect within grace, automatic ending beyond
grace, and broker/gateway final settlement. Source tests cannot certify DNS,
certificates, real GPU encoding or a paid deployment.

## Production public-origin validation

The NVIDIA production compose profile defaults `LIVE_RUNNER_REQUIRE_HTTPS=true`
and requires an explicit `LIVEPEER_PUBLIC_URL`. Startup rejects an HTTP origin,
a missing hostname, credentials, query, or fragment. Generic development profiles
default the check to false for local HTTP smoke tests. This is configuration
validation, not a TLS listener or a certificate/reachability check (`runners-0nw`).

Before deploying the production profile, provision the TLS proxy described above
and set a real public HTTPS origin. Keep the internal runner listener HTTP behind
the proxy. Setting an `https` URL on the existing plain-HTTP port 18280 does not
enable TLS. The proxy must forward public HLS and grant-protected stream-key/status
routes while excluding management routes. Validate a fresh descriptor, playlist,
segments, and key issuance from the gateway network. Existing HTTP-only production
configuration now fails startup intentionally; configure the edge before rollout.

## Recovering a lost live create response

Upgrade the Modules broker to support optional `paths.reconcile` before attaching
this runner release. The broker calls the runner over its existing private
authenticated path using the original broker session ID. Recovery identifies
and terminates created work, or fences absent work before releasing a slot.
No request replay should be used to discover whether uncertain work exists.

Keep the live state volume and sealing key intact. Alongside session JSON files,
`*.create-fence` files are permanent integrity-protected create exclusions and
must not be pruned. An invalid fence fails closed. Do not delete state or change
keys to recover apparent capacity; doing so removes the delayed-create guard.
Receiver accounting and broker-signed settlement remain the broker's obligation.
