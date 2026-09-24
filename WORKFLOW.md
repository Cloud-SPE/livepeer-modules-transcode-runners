# Work tracking

Beads (`bd`) is the source of truth for all project work: tasks, bugs, features,
investigations, documentation, dependencies, progress, and handoffs. Design
documents remain in Markdown and are linked from beads. Do not maintain a
second task list in Markdown or another agent planning tool.

## Setup

The project uses `bd` 1.2.2 with embedded Dolt and the custom
[Beads skill](./.agents/skills/beads/SKILL.md). The skill and its references are
vendored so every checkout uses the same instructions without depending on a
developer's absolute filesystem path.

Install `bd` using the skill's
[installation reference](./.agents/skills/beads/references/01-installation-and-init.md).
After cloning this repo, restore its tracker and install local Git hooks:

```bash
bd version
bd bootstrap
bd hooks install
bd prime
bd ready --json
```

The initial tracker must be published with `bd dolt push` before another clone
can recover its issues from the remote. A Git clone alone does not copy the
database. Until that first push, the database exists only in the initializing
workspace. Check the bootstrap result before assuming an empty tracker is
correct.

The checked-in `.codex/hooks.json` loads Beads context at session start and
refreshes it after compaction. `.codex/config.toml` enables hooks. The manual
`bd prime` requirement in `AGENTS.md` also applies when a client does not run
hooks. `CLAUDE.md` points to the same repo instructions.

Do not run plain `bd init` or `bd setup codex` over this setup: those commands
can overwrite the custom skill. Initialization of a genuinely new project
uses `bd init --prefix runners --skip-agents --non-interactive`; existing
checkouts use `bd bootstrap`.

## Daily workflow

At session start, or after context compaction:

```bash
bd prime
bd ready --json
bd list --status in_progress --json
```

Search for an existing bead before creating one. Every new bead needs a
description explaining scope and what completion means. Claim it before edits:

```bash
bd search "short description" --json
bd create "Describe the work" -t task -p 2 \
  --description "Scope, relevant files, and acceptance criteria" --json
bd show <id> --json
bd update <id> --claim
```

Record findings with `bd note <id> "..."`. Add real prerequisites with
`bd dep add <dependent> <blocker>` and discovered follow-ups with
`bd create ... --deps discovered-from:<id>`. Use an epic and child beads for
larger efforts. Never use interactive `bd edit` in an agent session.

Run the relevant Docker-first checks before closing implementation work.
Tracking and documentation changes need CLI/configuration checks; they do
not require rebuilding runner images. Close completed beads with evidence:

```bash
bd close <id> --reason "What changed and how it was verified"
bd ready --json
git status --short
```

For unfinished work, add a handoff note with current findings, blockers, and
the next action. Review `needs-status-review` imports against current code;
close completed or superseded work with evidence and remove that label after
review. Imported descriptions preserve the old design and may be outdated.

## Persistence and sharing

Track `.beads/` configuration, metadata, hooks, and agent instructions in Git.
The actual embedded database and runtime files remain ignored. The configured
Dolt `origin` points to this project's GitHub repository; Beads uses a separate
data ref. JSONL exports are optional interchange, not the live database or sync.

When publishing is authorized, synchronize with `bd dolt pull` / `bd dolt push`
as appropriate, and commit/push the repository changes separately. Follow the
authority printed by `bd prime`; this setup keeps the conservative default.
Local tracking does not require permission for every bead update. Report
unpublished work at handoff rather than claiming a Git commit synced issues.

## Updating the skill

The source is `/home/mazup/git-repos/personal_brand/beads-skill`, revision
`b59af466e6724b861823b823ac75cf87e52d34c9`. Copy updates deliberately into this
repo and review the diff:

```bash
cp -R /home/mazup/git-repos/personal_brand/beads-skill/skills/beads/. .agents/skills/beads/
cp /home/mazup/git-repos/personal_brand/beads-skill/{LICENSE,NOTICE.md,BEADS_VERSION.md} .agents/skills/beads/
git diff -- .agents/skills/beads/
```

Check for removed upstream files as well, update the recorded revision, and
verify compatibility with `bd version` and `bd help <command>`. Keep the license
and attribution with the skill. Updating these instructions does not upgrade
the `bd` binary or migrate its database.
