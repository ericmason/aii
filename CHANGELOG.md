# Changelog

All notable changes to aii are documented here. Format follows [Keep a
Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). aii is
pre-1.0, so minor bumps may carry small breaking changes — those are
called out under **Changed** when they happen.

## [Unreleased]

## [0.5.0] - 2026-05-01

### Added
- `aii search`, `aii sessions`, and `aii related` now accept
  `--format pretty|json|ndjson` as an alternative to the
  `--pretty`/`--json`/`--ndjson` boolean flags.
- `aii show` now accepts `--ndjson`, `--json`, and `--pretty` boolean
  flags as aliases for `--format ndjson` / `--format ndjson` /
  `--format md`. `aii show <cite> --ndjson` no longer errors with
  `flag provided but not defined: -ndjson`.

### Changed
- `aii show --format` also tolerates `pretty`/`human` (→ `md`) and
  `json` (→ `ndjson`) so chained `search → show` works regardless of
  which format vocabulary the caller reaches for.

## [0.4.2] - 2026-05-01

### Fixed
- `aii show` and `aii related` now accept the full cite-token grammar
  (`<agent>/<short_uid>:<ordinal>`) that the skill docs and `aii help
  --json` already promised. Previously only a bare 8-char short_uid
  resolved; the prefixed form returned "session not found". When a
  cite carries a trailing `:<ordinal>` and no slice flag is set,
  `aii show` uses it as the implicit `--around` anchor.

## [0.4.1] - 2026-04-30

### Added
- `aii version` subcommand and top-level `--version` / `-v` / `-V`
  flags for quick version checks without parsing help text.

### Changed
- The aii skill now documents the `--ended-mid-task` filter on
  `aii sessions`.

## [0.4.0] - 2026-04-30

### Added
- `aii sessions` — browse sessions by metadata (time window, agent,
  workspace) without a full-text query. Pairs with `--since` /
  `--until` / `--order` / `--limit` / `--offset`.
- `--ended-mid-task` flag on `aii sessions` to surface only sessions
  whose final message was from a user or tool (interrupted or
  unfinished work).

## [0.3.2] - 2026-04-20

### Added
- Stale-index nudge: commands that read the index now warn when it
  hasn't been refreshed recently, prompting the user to run
  `aii index`.

## [0.3.1] - 2026-04-20

### Added
- Windows support for `aii cron install` via Task Scheduler
  (`schtasks`), matching the launchd / systemd paths on macOS / Linux.
- Cross-platform release workflow that builds and uploads binaries for
  macOS, Linux, and Windows on every tag.

## [0.3.0] - 2026-04-20

### Added
- Secret redaction during indexing: API keys, tokens, and PEM blocks
  are scrubbed from message content, titles, and summaries before
  being written. On by default; opt out with `aii index --no-redact`.
  `aii index --redact-sources` also rewrites JSONL transcripts in
  place (Cursor's SQLite store is skipped).
- Claude Code skill bundled under `skills/aii/SKILL.md` so agents
  using aii get the cite-token grammar and command surface
  automatically.
- Initial test coverage for the store and redaction packages, plus a
  Dependabot config for Go module updates.

### Changed
- `aii search` and friends no longer auto-run the indexer before
  querying. Indexing is now purely on-demand (`aii index`) or via the
  background cron, so reads are predictable and don't acquire the
  SQLite write lock.

### Internal
- The indexer ignores per-session `HANDOFF.md` notes so they don't
  pollute search results.

## [0.2.0] - 2026-04-17

### Added
- Initial public release of aii: index Claude Code, Codex, and Cursor
  transcripts into a local SQLite FTS5 database; search across all
  three; `aii show` / `aii ask` / `aii related` / `aii tui` /
  `aii serve` / MCP server over stdio.
- CI workflow and `scripts/release.sh` for tagging releases.

[Unreleased]: https://github.com/ericmason/aii/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/ericmason/aii/compare/v0.4.2...v0.5.0
[0.4.2]: https://github.com/ericmason/aii/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/ericmason/aii/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/ericmason/aii/compare/v0.3.2...v0.4.0
[0.3.2]: https://github.com/ericmason/aii/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/ericmason/aii/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/ericmason/aii/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/ericmason/aii/releases/tag/v0.2.0
