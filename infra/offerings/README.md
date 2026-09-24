# Runner attachment references

The YAML files here summarize each runner's declared contract for operators.
They are not broker catalog or pool template input. Fetch the authoritative
`/.well-known/livepeer-runner` document from the running image when attaching.

Use current Modules templates `templates/video-transcode-vod.yaml`,
`templates/video-transcode-abr.yaml` and `templates/video-transcode-live.yaml`
for price, capacity, session funding policy and certification. Do not derive
pricing or protocol metadata from the legacy `methods`/`status_method` files.
