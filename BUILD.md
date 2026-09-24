# Build

All commands run from the repository root. No host Go or Node installation is
needed. Docker must be running.

```sh
./build-images.sh test
./build-images.sh validate
TAG=v2-local ./build-images.sh build
```

The full build orders codecs, each vendor FFmpeg base, all nine runners, and
the tester. For an existing base, build selected runners with:

```sh
TAG=v2-local ./build-images.sh build abr-runner-nvidia live-runner-nvidia
```

`REGISTRY` defaults to `tztcloud`; `INTERNAL_REGISTRY` to `localbuild`.
`TAG` defaults to `v2-local`, a local migration tag, not a published release.
Set a reviewed release tag explicitly for production. `GO_VERSION` defaults
to 1.25.7, `CUDA_VERSION` to 12.8.1, `UBUNTU_VERSION` to 24.04 and
`NODE_VERSION` to 22. All Dockerfiles are under `infra/dockerfiles/` with root
build context. Live images include MediaMTX pinned by digest.

`go-runner.Dockerfile` builds VOD and ABR package roots and
`live-runner/cmd/live-runner`. It creates writable persistent-state boundaries,
installs non-root runtime users, and sets live hardware policy per image.
NVIDIA remains CUDA 12.x because Pascal/sm_61 support is required. Intel and
AMD use their dedicated userspace drivers and `/dev/dri` access.

Publishing is an explicit operator action: `TAG=<release> ./build-images.sh
push`. This migration does not publish or deploy images.
