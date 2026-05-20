# BUILD

All Dockerfiles build from repo root and live under [`infra/dockerfiles/`](./infra/dockerfiles/).

## Images

Internal-only build artifacts:

- `codecs-builder`
- `ffmpeg-base-nvidia`
- `ffmpeg-base-intel`
- `ffmpeg-base-amd`

Runner images:

- `transcode-runner-nvidia`
- `transcode-runner-intel`
- `transcode-runner-amd`
- `abr-runner-nvidia`
- `abr-runner-intel`
- `abr-runner-amd`
- `live-runner-nvidia`
- `live-runner-intel`
- `live-runner-amd`
- `transcode-tester`

## Script

Use [`build-images.sh`](./build-images.sh):

```bash
./build-images.sh build
./build-images.sh build ffmpeg-base-nvidia transcode-runner-nvidia
./build-images.sh push
./build-images.sh validate
./build-images.sh clean
```

Key environment overrides:

- `REGISTRY` default `tztcloud`
- `INTERNAL_REGISTRY` default `localbuild`
- `TAG` default `v1.3.0`
- `CUDA_VERSION` default `13.2.1`
- `UBUNTU_VERSION` default `24.04`
- `GO_VERSION` default `1.25.7`
- `NODE_VERSION` default `22`

Runner images also embed startup build metadata:

- `version`
- `commit`
- `build time`

Those fields are stamped through linker flags during `./build-images.sh build` and
show up in runner startup logs.

## CUDA 13 note

The NVIDIA build targets CUDA `13.2.1`.

- CUDA / NVENC / NVDEC / CUVID are enabled
- `libnpp` is intentionally disabled in the current FFmpeg pin because CUDA 13 removed legacy NPP entrypoints that FFmpeg `7.1.3` still references
- FFmpeg base images now disable docs and static artifacts at build time, and runtime stages strip unneeded `.a` files and `pkgconfig` metadata

## Lean runtime policy

Runtime images should include only:

- runner binary
- `ffmpeg`
- `ffprobe`
- required shared libraries
- CA certificates
- non-root user setup

Toolchains, build utilities, and package managers stay in builder stages.

`build-images.sh push` only publishes deployable runner images. Base images and
`codecs-builder` stay under the internal local build namespace and are not
tagged into `tztcloud/*`.
