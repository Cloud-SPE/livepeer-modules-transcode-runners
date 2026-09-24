# Security

Runners do not authenticate customers or validate payment. The Modules broker
owns that boundary. Keep runner invocation/control routes on the operator
network. Modules member-agent tunnel attachment provides the trust boundary
without a runner bearer. For standalone direct clients, configure the live
broker bearer on both sides.

Signed input/output URLs, callback tokens, storage credentials, grant secrets
and stream keys are credentials. Never log request bodies or commit real env
files. Public status and runtime descriptors contain only safe coordinates;
key issuance requires the scoped grant bearer. An identical key request may
return the same key for recovery; a new request rotates and invalidates it.

Live state encrypts per-session credentials under a stable operator master
key. Protect and back up state with its key. MediaMTX APIs and internal media
listeners remain loopback-only. Runtime images run non-root. Treat all media
URLs as untrusted and enforce network policy around runner egress.
