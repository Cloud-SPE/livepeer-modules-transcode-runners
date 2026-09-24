# Notice and attribution

`LICENSE` covers **this repository only** — the skill text, scripts, and
supporting docs. It is deliberately kept as unmodified MIT text so automated
license detection works; the attribution that would otherwise sit there lives
in this file instead.

## Upstream project

This repository contains an agent skill **about** beads. It does not contain,
vendor, or redistribute the beads source code.

- **Project:** beads (`bd`)
- **Upstream:** https://github.com/gastownhall/beads
- **Created by:** Steve Yegge
- **License:** MIT — © 2025 Beads Contributors
- **Documentation site:** https://beads.gascity.com/

The upstream repository is referenced here as a git submodule at `beads/`. Its
contents are fetched at checkout time from the URL above and are governed by
its own MIT license — nothing from it is committed into this repository's
history beyond the submodule pointer.

## What this repository contains

`skills/beads/` is an independently written work derived from beads' public
documentation (`docs/`), its generated CLI reference, and the observed
behaviour of the `bd` binary at the versions recorded in `BEADS_VERSION.md`.
It paraphrases and reorganizes that material for agent consumption; command
names, flag spellings, configuration keys, error codes, and file paths are
necessarily reproduced verbatim, since they are the interface being documented.

Short illustrative snippets (command invocations, TOML formula fragments, JSON
shapes) follow upstream's documented forms. Longer prose is original.

## MIT license (upstream)

```
MIT License

Copyright (c) 2025 Beads Contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

The canonical license text is `beads/LICENSE` in the submodule; the copy above
is included so this notice stands alone when the submodule is not checked out.

## No affiliation

This skill is not produced, endorsed, or reviewed by the beads maintainers.
Where this skill and `bd help` disagree, `bd` is correct and the skill needs
updating.
