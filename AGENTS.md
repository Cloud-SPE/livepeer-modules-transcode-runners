# AGENTS.md

This repo is the standalone home for the video transcode runners that the
Livepeer capability broker forwards paid jobs to.

This file is a map, not a manual.

## Operating principles

Inherited from the harness reference in
[`docs/references/openai-harness-engineer.md`](./docs/references/openai-harness-engineer.md):

- Humans steer; agents execute.
- The repo is the system of record.
- Progressive disclosure beats giant manuals.
- Enforce invariants, not implementations.
- Favor fast fix-forward loops over ceremony.

Repo-specific principles:

- Runners are blind to customer identity, billing, and payment validation.
- The repo is clean-slate. Do not reintroduce source-monorepo history as docs.
- All build/test gestures are Docker-first.
- Dockerfiles live under `infra/dockerfiles/` and always build from repo root.
- Runtime images should be lean and run as non-root users.
- NVIDIA, Intel, and AMD support are all first-class.

## Where to look

| Question | File |
|---|---|
| What is this repo? | [`README.md`](./README.md) |
| One-page architecture | [`DESIGN.md`](./DESIGN.md) |
| How images build | [`BUILD.md`](./BUILD.md) |
| HTTP/API contract | [`API.md`](./API.md) |
| Runtime operations | [`OPERATIONS.md`](./OPERATIONS.md) |
| Test strategy | [`TESTING.md`](./TESTING.md) |
| Security expectations | [`SECURITY.md`](./SECURITY.md) |
| Work tracking and Beads setup | [`WORKFLOW.md`](./WORKFLOW.md) |
| Harness reference | [`docs/references/openai-harness-engineer.md`](./docs/references/openai-harness-engineer.md) |

## Components

- `transcode-runner/` — single-rendition VOD transcode runner
- `abr-runner/` — ABR ladder runner
- `live-runner/` — remote live RTMP ingest + HLS session runtime
- `transcode-core/` — shared FFmpeg/GPU/HLS/preset logic
- `transcode-tester/` — direct-runner smoke harness
- `infra/dockerfiles/` — shared base and runner Dockerfiles
- `infra/compose/` — runtime overlays
- `infra/env/` — env templates
- `infra/offerings/` — offering manifests
- `infra/presets/` — operator-editable preset YAMLs

## Doing work in this repo

- Use Beads (`bd`) for all work tracking: features, bugs, chores, investigations,
  documentation, plans, and discovered follow-ups. Read the project
  [Beads skill](./.agents/skills/beads/SKILL.md) before starting work.
- Run `bd prime` at session start and after compaction, then inspect
  `bd ready --json` and `bd list --status in_progress --json`.
- Create or find a bead before changing files, claim it with
  `bd update <id> --claim`, and record progress, dependencies, and validation
  there. Close completed work with a reason; leave unfinished work with handoff
  notes. Use `--json` for output that you parse.
- Beads is the only task/status tracker. Keep design and reference documents in
  Markdown, linked from beads; do not maintain parallel Markdown backlogs,
  TODO lists, or plan progress checklists. This overrides the reference
  article's Markdown tracking examples.
- Review imported beads labeled `needs-status-review` against current code
  before implementing them; their original backlog had no completion status.
- If `bd` is unavailable, repair/bootstrap it using `WORKFLOW.md` before
  starting tracked work. Do not silently fall back to another tracker.

- Build all images with `./build-images.sh build`
- Validate compose overlays with `./build-images.sh validate`
- Use `REGISTRY=`, `TAG=`, `CUDA_VERSION=`, `GO_VERSION=`, and `NODE_VERSION=` to override defaults
- Treat `infra/presets/` as operator-facing configuration; the runner dirs keep embedded defaults
- Keep docs at the repo root unless the file is an exec-plan or reference artifact under `docs/`
