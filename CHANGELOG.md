# Changelog

All notable changes to aii are documented here. Format follows [Keep a
Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). aii is
pre-1.0, so minor bumps may carry small breaking changes — those are
called out under **Changed** when they happen.

## [Unreleased]

### Added
- One-line install script for macOS and Linux — no Go toolchain
  required: `curl -fsSL
  https://raw.githubusercontent.com/ericmason/aii/main/scripts/install.sh
  | sh` auto-detects your OS/arch, downloads the matching prebuilt
  binary from the latest GitHub release, verifies its SHA-256 checksum,
  and installs it to `~/.local/bin`. Override the target dir with
  `AII_INSTALL_DIR` or pin a version with `AII_VERSION`.
- Index and search sessions from four more agents. `aii index` picks them up by default, and `--source`/`--agent` accept their names. aii only reads their files and never modifies them.
  - **pi** (`--agent pi`, cite prefix `pi/`): `~/.pi/agent/sessions`, or `PI_CODING_AGENT_SESSION_DIR` / `PI_CODING_AGENT_DIR` when set. Session names and compaction summaries become the session title and summary. Short ids come from the random end of pi's session id, so sessions started in the same minute get different cite tokens; `aii show` also accepts the full id pi prints.
  - **opencode** (`--agent opencode` or `oc`, cite prefix `oc/`): `~/.local/share/opencode/opencode.db`, honoring `XDG_DATA_HOME` and `OPENCODE_DB`. The database is opened read-only, so indexing is safe while opencode is running.
  - **Gemini CLI** (`--agent gemini` or `gem`, cite prefix `gem/`): `~/.gemini/tmp/*/chats`, honoring `GEMINI_CLI_HOME`. Both the JSONL session log and the older single-file JSON format are read, and rewinds are respected.
  - **GitHub Copilot Chat in VS Code** (`--agent copilot` or `cop`, cite prefix `cop/`): chat sessions from VS Code, VS Code Insiders, and VSCodium, including chats opened without a folder. Each session's workspace is the folder VS Code had open.
- `aii doctor` lists the data paths it reads for the new agents, and the web UI and TUI can filter by them.

### Changed
- README install instructions now lead with the one-line installer and
  manual prebuilt-binary download (with a per-platform asset table);
  `go install` / `go build` are now documented as the "from source"
  path for Go developers rather than the only option.
- `aii index --redact-sources` prints a note and skips pi, opencode, Gemini CLI, and Copilot Chat, because aii does not rewrite those agents' files.

### Fixed
- `aii search` no longer fails with `fts5: syntax error near "*"` when
  the query contains an uppercase `AND`, `OR`, or `NOT`. These words
  now work as boolean operators between terms, so
  `aii search 'download OR extract'` matches either word and
  `aii search 'extract NOT download'` excludes the second. Lowercase
  `and`/`or`/`not` are still ordinary search terms. A query that starts
  with `NOT` matches nothing instead of everything containing the
  excluded word.
- A cite token such as `cdx/019a1b2c:12` no longer opens the wrong
  session. Codex session ids start with a timestamp, so sessions
  started within about a minute of each other shared the same 8-char
  short id, and `aii show`, `aii related`, and the MCP `get_session`
  and `related` tools opened an arbitrary one of them. Cite tokens now
  use the shortest id prefix that matches only one session (8 chars
  for most sessions, longer for those that would collide). An id that
  still matches several sessions is now an error that lists each
  match with its agent, id, start time, and title. A token you saved
  earlier still opens its session when it is that session's full id,
  or when no other session's id starts with the same characters.
  The token's agent code now picks the session when two agents share
  an id, and id matching ignores case again. `aii ui` shows the same
  short ids, and its preview opens the session you clicked even when
  another agent has one with the same id; so does the `aii tui`
  preview.
- `aii search` no longer hangs when the query contains a newline or
  carriage return.
- Claude Code sessions keep their original title when new turns are indexed. Before, each incremental index run retitled the session with the first user message it had just read.
- A Claude Code transcript line larger than 16 MiB no longer stops indexing of that file. aii logs a warning, skips the line, and indexes everything after it; before, nothing past that line was ever indexed.
- Indexing a Claude Code transcript while Claude Code is writing to it no longer drops or duplicates messages. aii now waits for a line to be complete before indexing it, and leaves lines written during the index run for the next run.
- `aii search` returns in seconds instead of minutes for common words
  such as `app` or `download` on a large index. Search now builds
  excerpt snippets only for the sessions it returns, not for every
  matching message, and looks up each match's session through a new
  index. The first command after upgrading builds that index once,
  which can take a minute or two on a multi-gigabyte index. Other aii
  commands keep working while it builds; their searches skip the index
  until it is ready.

## [0.5.1] - 2026-05-27

### Added
- `aii show --last --workspace DIR` (use `.` for the current directory)
  picks the most recent session in that workspace — handy when a fresh
  agent wants to pick up where the last one left off. By default the
  command skips any session whose transcript was touched in the last
  ~30 seconds, so a newly-started agent doesn't see its own in-flight
  session; pass `--include-active` to include it. The live file mtime
  is stat'd at call time so the filter works even when the index is
  minutes stale.

### Changed
- `--workspace` on `aii search`, `aii sessions`, `aii ask`, and the new
  `aii show --last --workspace` all resolve relative paths (including
  `.`) against the current directory. Previously `--workspace .`
  silently matched nothing because the index stores absolute paths.

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

[Unreleased]: https://github.com/ericmason/aii/compare/v0.5.1...HEAD
[0.5.1]: https://github.com/ericmason/aii/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/ericmason/aii/compare/v0.4.2...v0.5.0
[0.4.2]: https://github.com/ericmason/aii/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/ericmason/aii/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/ericmason/aii/compare/v0.3.2...v0.4.0
[0.3.2]: https://github.com/ericmason/aii/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/ericmason/aii/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/ericmason/aii/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/ericmason/aii/releases/tag/v0.2.0
