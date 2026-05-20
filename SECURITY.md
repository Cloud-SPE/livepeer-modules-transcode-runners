# SECURITY

- No secrets, keystores, or operator-local state belong in this repo
- Runtime images should run as non-root users
- Only the minimal runtime packages should ship in final images
- Upload and download URLs are caller-controlled inputs and should be treated as untrusted
- This repo does not own customer auth, payment validation, or billing logic
- `live-runner` control APIs should be protected with broker-scoped auth when exposed on a shared network
- `live-runner` stream keys are bearer secrets and should only be returned once on session creation

If security-sensitive operator configuration is needed, provide `.env.example`
templates under `infra/env/` and keep real values out of git.
