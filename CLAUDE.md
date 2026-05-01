# CLAUDE.md

Project-specific rules for Claude Code working in this repo. See
`AGENTS.md` for the broader contributor guide (architecture, source
layout, how to add a new agent source, etc.).

## Always update CHANGELOG.md

Every user-visible change must add an entry under the `## [Unreleased]`
section of `CHANGELOG.md` in the same commit. This includes:

- New commands, flags, or output modes.
- Behavior changes (defaults, formats, semantics).
- Bug fixes that a user could observe.
- Documentation changes that affect the user-facing surface (e.g. the
  aii skill, `aii help --json`).

Skip the changelog only for: pure internal refactors with no observable
effect, test-only changes, and CI / tooling tweaks. When in doubt,
write the entry.

Use the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
sections (`Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`,
`Security`). Write entries from the user's perspective — what they can
now do, or what no longer breaks — not what the diff touched.

At release time, `scripts/release.sh` will rename `## [Unreleased]` to
`## [X.Y.Z] - YYYY-MM-DD` and insert a fresh empty `[Unreleased]`
heading. The script refuses to release if `[Unreleased]` is empty, so
the rule is self-enforcing.

## Install after every release

Immediately after `scripts/release.sh` succeeds, rebuild and install
the binary to the path the user actually runs from:

```bash
go build -o ~/.local/bin/aii ./cmd/aii
aii version   # confirm the new version is live
```

The release script tags GitHub but does not touch the local binary,
and `~/.local/bin/aii` (not `~/go/bin/aii`) is what's on PATH. Skipping
this step leaves the user invoking the old version and makes any
post-release smoke test misleading.
