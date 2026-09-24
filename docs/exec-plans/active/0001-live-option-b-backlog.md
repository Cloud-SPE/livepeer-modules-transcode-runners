---
plan: 0001-backlog
title: Live Option B backlog migration index
status: migrated
beads_epic: runners-x8e
---

# Live Option B — Beads migration index

The backlog moved to Beads on 2026-09-24. This is a static ID lookup, not a
live task list. Use `bd show runners-x8e --json` for the epic and
`bd list --parent runners-x8e --limit 0 --json` for its children.

All 22 local items and two external dependencies retain their original scope
and acceptance text in Beads, with 32 blocking dependency edges. Imported
items carry `needs-status-review`: their old Markdown had no completion
status, and the current runner already implements parts of the design.
Review the code before implementing or closing an item.

The [original design](./0001-live-option-b-remote-runner.md) remains reference
material. Suggested batches A–D and the B001–B016 milestone are historical
planning guidance, not new dependencies. Current progress and decisions
belong in the epic and its children. See [WORKFLOW.md](../../../WORKFLOW.md).

| Original ID | Bead | Original title |
|---|---|---|
| B001 | `runners-x8e.2` | Scaffold `live-runner/` |
| B002 | `runners-x8e.3` | Session model and in-memory store |
| B003 | `runners-x8e.4` | Create/get/delete HTTP API |
| B004 | `runners-x8e.5` | Broker auth for runner control API |
| B005 | `runners-x8e.6` | RTMP ingest listener scaffold |
| B006 | `runners-x8e.7` | Stream key generation and publish auth |
| B007 | `runners-x8e.8` | HLS scratch/output manager |
| B008 | `runners-x8e.9` | FFmpeg live command integration |
| B009 | `runners-x8e.10` | FFmpeg supervisor and session lifecycle wiring |
| B010 | `runners-x8e.11` | Minimal HLS serving for smoke/dev |
| B011 | `runners-x8e.12` | Usage counter abstraction |
| B012 | `runners-x8e.13` | Broker callback client scaffold |
| B013 | `runners-x8e.14` | Emit `session.started` and `session.heartbeat` |
| B014 | `runners-x8e.15` | Emit `session.usage.tick` |
| B015 | `runners-x8e.16` | Emit terminal events |
| B016 | `runners-x8e.17` | Broker-forced terminate path |
| B017 | `runners-x8e.18` | Timeouts and watchdogs |
| B018 | `runners-x8e.19` | Metrics |
| B019 | `runners-x8e.20` | Docker image and build wiring |
| B020 | `runners-x8e.21` | Compose/env examples |
| B021 | `runners-x8e.22` | Runner smoke harness |
| B022 | `runners-x8e.23` | Root docs update |
| E001 | `runners-x8e.24` | Broker contract finalized |
| E002 | `runners-x8e.25` | Gateway live adapter migration |
