# aii — search every AI chat you've had locally

[![CI](https://github.com/ericmason/aii/actions/workflows/ci.yml/badge.svg)](https://github.com/ericmason/aii/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

`aii` indexes every conversation from your local AI coding agents into a
SQLite FTS5 database and makes them searchable from the CLI, a minimal
TUI, a local web UI, or — most importantly — as a tool your coding
agents can call directly over MCP.

Three things keep it useful:

- **One static binary.** Pure-Go SQLite (`modernc.org/sqlite`). No cgo,
  no extensions to load, no Python sidecar.
- **Zero network.** Nothing leaves your machine. `aii ask` optionally
  shells out to a local LLM CLI (`claude`, `codex`, `ollama`), but only
  when you ask it to.
- **Agent-native.** A stdio MCP server, stable cite tokens, ndjson
  output on pipes, token budgets, and sliced reads — so an agent can
  drive it without regex-parsing pretty tables.

Indexed sources:
- **Claude Code** — `~/.claude/projects/*/*.jsonl`
- **Codex** — `~/.codex/sessions/**/rollout-*.jsonl` + `~/.codex/history.jsonl`
- **Cursor** — `~/Library/Application Support/Cursor/User/{workspaceStorage,globalStorage}/state.vscdb`

## Install

```sh
go install github.com/ericmason/aii/cmd/aii@latest
```

Or clone and build locally:

```sh
git clone https://github.com/ericmason/aii.git
cd aii
go build -o ~/.local/bin/aii ./cmd/aii
```

That's it. The database lives at `$AII_DB` or
`~/.local/share/aii/aii.db`, created on first run.

## Quick start

```sh
aii index                      # scan every source, incrementally
aii search "webhook retry"     # find conversations that touched it
aii sessions --since 7d        # browse everything from the last week
aii show <session-uid>         # print the whole session
aii ask  "how did I fix the webhook retries?"  # RAG over your chats
```

Run `aii help` for a command list, or `aii help --json` to dump a
machine-readable schema (commands, flags, cite-token grammar).

## Commands

### `aii index` — build/update the index

```
aii index [--source all|cc|codex|cursor] [--full] [--verbose]
          [--no-redact] [--redact-sources]
```

Each source parses its own format and emits batches of
`(session, new-messages)`. JSONL sources tail-follow by byte offset;
Cursor composers are fingerprinted by bubble count, so only new bubbles
are re-read. A single writer goroutine applies batches inside
per-session transactions.

Incremental by default; `--full` wipes state and reindexes. `--verbose`
logs each session as it's written and runs an `ANALYZE` at the end.

**Redaction.** By default, common secret shapes (Anthropic/OpenAI/GitHub
tokens, AWS access keys, JWTs, PEM-armored private keys, passwords in
URLs, Authorization headers, …) are scrubbed out of message content
*before* it lands in the index, replaced with `[REDACTED:<label>]`.
Pass `--no-redact` to keep raw content. Pass `--redact-sources` to also
rewrite the underlying transcript files in place — this is destructive
and only touches JSONL sources (Claude Code + Codex); Cursor's SQLite
store is skipped. The list of patterns lives in
[`internal/redact/redact.go`](internal/redact/redact.go); add to it if
your workflow has a proprietary token shape.

### `aii search` — hybrid full-text search

```
aii search <query> [--agent cc|codex|cursor] [--workspace DIR] [--role user|assistant|tool|thinking]
                   [--since 7d|2026-01-01] [--limit 20] [--offset 0]
                   [--pretty|--ndjson|--json] [--agent-mode] [--max-bytes N]
```

Searches two FTS5 indexes in parallel (see **Query syntax** below),
dedupes by message id, returns one `Result` per session with up to 3
top excerpts each.

Output mode resolution:
- `--json`, `--ndjson`, `--pretty` win explicitly, in that order.
- If none: **pretty** on a TTY, **ndjson** when piped.
- `--agent-mode` or `AII_AGENT=1` force ndjson regardless.

### `aii sessions` — browse sessions by time / agent / workspace

```
aii sessions [--agent cc|codex|cursor] [--workspace DIR]
             [--since 7d|2026-01-01] [--until 1d|2026-01-15]
             [--limit 50] [--offset 0] [--order asc|desc]
             [--pretty|--ndjson|--json] [--max-bytes N]
```

The "what was I working on last week?" entry point. No full-text
query — just a metadata listing of sessions ordered by time, with
title, workspace, and message count per session. Pair with
`aii show <uid>` to open any thread, or pipe to your shell:

```sh
aii sessions --since 7d                           # last week
aii sessions --since 30d --until 7d --agent cc    # 3-week window, Claude Code only
aii sessions --workspace "$HOME/code/myproject"   # everything in one repo
aii sessions --since 1d --ndjson | jq .uid        # pipe-friendly
```

### `aii show` — print a session (full or sliced)

```
aii show <uid>|--last [--format md|plain|ndjson]
                      [--around N --span M] [--from N --to M]
                      [--role ...] [--max-msg-chars N] [--max-bytes N]
```

Accepts a full uid or the 8-char prefix. Slicing options:
- `--around 42 --span 3` — ±3 messages around ordinal 42.
- `--from 40 --to 60` — inclusive ordinal range.
- Neither — full session.

`--role user` is great for "remind me what I was asking about."
`--max-msg-chars` truncates each message; `--max-bytes` caps the total
output. `ndjson` format emits one JSON object per message with a cite
token.

### `aii ask` — retrieval-augmented Q&A over your corpus

```
aii ask <question> [--k 6] [--context 2]
                   [--agent ..] [--workspace ..] [--since ..]
                   [--cmd "claude -p"] [--dry-run] [--show-sources]
                   [--max-msg-chars 4000]
```

Runs `search`, pulls ±`context` messages around each hit's best
excerpt, builds a grounded markdown prompt with citation instructions
(`[uid:ordinal]`), and pipes it to a local LLM CLI.

Command resolution: `--cmd` → `$AII_ASK_CMD` → `claude -p` if on PATH →
`codex exec` if on PATH → error.

`--dry-run` prints the prompt to stdout instead of running anything;
use it for composition: `aii ask "..." --dry-run | ollama run llama3`.

### `aii related` — pivot to similar sessions

```
aii related <uid> [--limit 10] [--agent ..] [--since ..]
                  [--json|--ndjson|--pretty] [--max-bytes N]
```

Uses the source session's title (falling back to summary, then first
substantive user message) as a query seed and runs hybrid search with
the source uid excluded. Good for "I remember discussing this across a
few threads — find them all."

### `aii mcp` — run as an MCP server

```
aii mcp
```

Stdio JSON-RPC 2.0 MCP server. Exposes four tools: `search`,
`get_session`, `related`, `stats`. See **Using from coding agents**
below.

### `aii stats` / `aii doctor`

`stats` prints session/message counts per agent and the latest
timestamp. `doctor` shows the DB path, source paths, and per-agent
counts — run it first if something looks off.

### `aii serve` / `aii ui` / `aii tui`

Local web UI (`serve` binds to 127.0.0.1:8723 by default, `ui` also
opens your browser) and a two-pane bubbletea TUI (`tui`). These are
human-facing; agents should prefer MCP or the CLI.

## Query syntax

`aii search` hits two FTS5 indexes and unions the results:

- **`messages_fts`** — `tokenize='porter unicode61 remove_diacritics 2'`.
  Good at natural language. Ranks by bm25.
- **`messages_fts_tri`** — `tokenize='trigram case_sensitive 0'`. Finds
  any 3+ char substring. Essential for code identifiers
  (`SessionUID`, `snake_case`, `normalizeMatch`) that porter splits on
  case/underscore boundaries.

A hit in both tables is deduped to its best score; trigram scores are
scaled by 0.7 so porter wins ties. Queries shorter than 3 chars skip
the trigram branch entirely.

Query-string rules (applied by the normalizer):
- Bare words become prefix terms — `webhook auth` → `webhook* auth*`
  on porter, `"webhook" "auth"` on trigram.
- Double-quoted phrases are preserved verbatim — `"api key rotation"`.
- Non-alphanumeric chars are stripped from bare words, so FTS5 special
  syntax (`AND`, `OR`, `NEAR`, `:`, parens) can't detonate on user
  input.

Examples:

```sh
aii search 'webhook retry'                 # natural language
aii search 'SessionUID'                    # CamelCase identifier (trigram wins)
aii search '"api key rotation"'            # exact phrase
aii search 'webhook' --agent cc --since 7d # filter
aii search 'deploy' --role user            # only what you asked
aii search 'flakey tests' --workspace "$HOME/code/myproject"
```

## Output modes & agent behavior

Every `aii` command that returns data supports the same three output
modes. The right one gets picked automatically most of the time.

| Mode   | How it renders                                            | Default when…        |
|--------|-----------------------------------------------------------|----------------------|
| pretty | Colored terminal output with tree glyphs and badges       | stdout is a TTY      |
| ndjson | One compact JSON object per line (excerpt/message/hit)    | stdout is piped      |
| json   | A single pretty-printed JSON array (whole result)         | `--json` requested   |

Override with `--pretty`, `--ndjson`, `--json`, `--agent-mode`, or the
`AII_AGENT=1` env var.

### Cite tokens

Every excerpt and every message aii emits carries a stable **cite
token** in the format:

```
<agent_code>/<short_uid>:<ordinal>
```

- `agent_code` — `cc` (Claude Code), `cdx` (Codex), `cur` (Cursor).
- `short_uid` — first 8 chars of the session UUID.
- `ordinal` — 0-indexed position of the message within the session.

Example: `cc/32e869ac:319`. You can hand this token back to `aii show`
or the MCP `get_session` tool as the session ref — the ordinal will be
used as the anchor if you don't pass one explicitly. Regex:

```
^(cc|cdx|cur)/[0-9a-f]{8}:\d+$
```

### Token budgets

`--max-bytes N` caps total output on search, show, and related. For
ndjson, emission stops at the last complete line under the budget and
a marker `{"truncated":true,"skipped_rows":N}` is appended, so the
stream stays parseable. For pretty and markdown, the output is trimmed
at the last newline within the budget and `…[truncated]` is appended.

`aii show --max-msg-chars 4000` bounds each message individually,
useful for sessions with huge tool-result dumps.

## Using from coding agents

### MCP (recommended)

```sh
# Claude Code
claude mcp add aii aii mcp

# Or manually, in ~/.claude.json (or equivalent for your client):
#   "mcpServers": { "aii": { "command": "aii", "args": ["mcp"] } }
```

Five tools. Full grammar is embedded in their descriptions — an agent
can discover the cite-token convention just by listing tools.

- `search(query, agent?, workspace?, role?, since?, limit?, offset?)`
  → hits with cite tokens.
- `list_sessions(agent?, workspace?, since?, until?, limit?, offset?, order?)`
  — browse sessions in a time window without a full-text query. Use
  for "what was I working on last week?" questions.
- `get_session(session, around?, span?, from?, to?, role?, max_msg_chars?)`
  — accepts full uid, short uid, or a full cite token. If the cite
  carries an ordinal and you don't pass `around`/`from`/`to`, the
  ordinal is used as the anchor automatically.
- `related(session, limit?, agent?, since?)` — pivot to similar
  sessions.
- `stats()` — sanity check.

Each tool returns both `structuredContent` (JSON) and a plain-text
fallback so every MCP client flavor works.

### From the CLI

Agents that shell out can just invoke `aii` directly. When stdout is
piped, aii switches to ndjson automatically — no flag needed. Typical
chain: `aii search` → pick a cite from the stream → `aii show --around
<ord>` to expand context.

```sh
aii search 'webhook retry' --limit 5 --max-bytes 8000
aii show cc/32e869ac:319 --span 3 --max-msg-chars 2000 --format ndjson
```

For agents that want the full command surface upfront, dump a schema:

```sh
aii help --json
```

See `AGENTS.md` for a condensed agent-facing manual with call patterns.

### Claude Code skill

This repo ships a Claude Code skill at `skills/aii/SKILL.md` that
teaches the agent when to search, the chain pattern (`search` →
`get_session`), cite-token grammar, and the query idioms that actually
work. Install it once:

```sh
# user-wide (all projects)
mkdir -p ~/.claude/skills
cp -R skills/aii ~/.claude/skills/aii

# or per-project
mkdir -p .claude/skills
cp -R skills/aii .claude/skills/aii
```

Claude Code auto-loads skills on startup and will trigger this one
whenever the user says "remember when…", "last time…", "how did I
fix…", etc. Pairs with either the MCP server or the CLI.

## Configuration

Environment variables:

| Var             | What it does                                             |
|-----------------|----------------------------------------------------------|
| `AII_DB`        | Override DB path. Default: `~/.local/share/aii/aii.db`. |
| `AII_AGENT`     | Force ndjson output regardless of TTY.                   |
| `AII_ASK_CMD`   | Default command for `aii ask` (e.g. `ollama run llama3`).|
| `NO_COLOR`      | Disable ANSI color in pretty output.                     |
| `FORCE_COLOR`   | Force ANSI color even when piped.                        |

## Architecture

```
cmd/aii/                 # CLI entry, color + tty helpers
internal/source/         # pluggable source.Source interface
internal/source/claudecode   # claude code jsonl parser
internal/source/codex        # codex rollouts + history parser
internal/source/cursor       # cursor sqlite storage parser
internal/indexer/        # orchestrates sources → single writer goroutine
internal/store/          # sqlite schema, search (hybrid FTS), migrations
internal/web/            # local HTTP UI
internal/tui/            # bubbletea TUI
internal/mcpserver/      # MCP stdio server (tools: search/get_session/related/stats)
```

Data flow: each `source.Source` emits `source.Batch`es. A single writer
goroutine applies them inside per-session transactions. Two FTS5
virtual tables (`messages_fts`, `messages_fts_tri`) are kept in sync
via triggers. `store.Search` UNIONs both, dedupes by message id, ranks
sessions by best bm25, and returns the top-K with up to 3 excerpts.

Schema lives in `internal/store/schema.go`. Two idempotent migrations
run on open: adding a `summary` column, and wiring the trigram triggers
(the second is the migration that also rebuilds the trigram shadow
index if it's empty).

## Troubleshooting

- **`aii search "SessionUID"` returns nothing.** The trigram index is
  probably empty. Check with `sqlite3 $DB "SELECT COUNT(*) FROM
  messages_fts_tri_docsize;"` — it should equal the `messages` count.
  If not, run `sqlite3 $DB "INSERT INTO messages_fts_tri(messages_fts_tri)
  VALUES('rebuild');"` or `aii index --full`.
- **`aii ask` says "no LLM command available".** Either `claude` / `codex`
  isn't on PATH, or set `AII_ASK_CMD` (e.g. `export AII_ASK_CMD="ollama
  run llama3"`). `--dry-run` always works without an LLM.
- **MCP server seems silent.** Stdio MCP is JSON-RPC, not human-facing.
  Confirm it's running with `echo '{"jsonrpc":"2.0","id":1,"method":"initialize",
  "params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":
  {"name":"smoke","version":"0"}}}' | aii mcp` — you should see a JSON
  response. Real errors go to stderr.
- **`aii doctor`** is the canonical "is everything wired up" check.

## Contributing

Issues and pull requests are welcome. The codebase is small and
documented — `AGENTS.md` has a condensed tour aimed at agents working
on this repo, but it's equally useful for humans.

## License

MIT — see [LICENSE](LICENSE).
