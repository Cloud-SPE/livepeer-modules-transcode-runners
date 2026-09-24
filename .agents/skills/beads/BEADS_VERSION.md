# Upstream pin

The skill in `skills/beads/` is written and verified against the versions
recorded here. Regenerate this file with `scripts/update-beads.sh`.

| Field | Value |
|---|---|
| Upstream repo | https://github.com/gastownhall/beads |
| Submodule path | `beads/` |
| Submodule commit | `7505e173f` |
| `git describe` | `v1.2.1-9-g7505e173f` |
| Docs CLI pin (`docs/cli-docs.pin`) | `v1.2.2` |
| `bd` binary verified against | `1.2.2` |
| Date written | 2026-08-15 |

## What "verified against" means

Command names, flags, and output shapes in the skill were taken from
`beads/docs/CLI_REFERENCE.md` at the submodule commit above, which upstream
generates from `bd help --all` at the `docs/cli-docs.pin` tag. Behaviour that
could drift (the `bd prime` contract, `bd create --json` output, the built-in
type and status lists, default hash-ID length) was additionally confirmed by
running `bd 1.2.2` against a disposable workspace.

## Known upstream doc drift at this pin

These are places where upstream prose disagrees with the generated reference or
the shipping binary. The skill follows the binary.

- `docs/index.md` still says "these docs are for the 1.1.0 release" while
  `docs/cli-docs.pin` is `v1.2.2`.
- `docs/core-concepts/hash-ids.md` says the default minimum hash length is 4;
  `docs/reference/configuration.md` and the 1.2.2 binary both use **3**.
- `docs/core-concepts/issues.md` lists five issue types; `bd types` on 1.2.2
  reports nine built-ins (adds `decision`, `spike`, `story`, `milestone`).
- `docs/cli-reference/index.md` says 108 top-level commands; `bd --help` on
  1.2.2 lists 109.
- **The prose docs describe commands that 1.2.2 does not have.** `docs/` lives
  on `main`, which is ahead of the release. `bd sync` (bucket-federation.md),
  `bd events` and `bd serve` (events-journal.md), and `bd reclaim` /
  `bd unclaim` / `bd heartbeat` (federation.md) are all registered in
  `cmd/bd/*.go` on main but rejected by the 1.2.2 binary with
  `Error: unknown command`. The skill marks each affected section rather than
  omitting it, since these should ship in a later release.
- Testing gotcha that produced a wrong answer once already: `bd help <unknown>`
  prints "Unknown help topic" and still **exits 0**, so
  `bd help foo && echo exists` is a false positive. Test with
  `bd foo --help 2>&1 | head -1` and look for `Error: unknown command`.
- The global `--dolt-auto-commit` flag help says the default is `off`, while
  `docs/reference/configuration.md` documents `dolt.auto-commit` defaulting to
  `on` for embedded mode. Treat auto-commit as on for embedded, off for server,
  and check `bd config show` rather than assuming.
