# AGENTS.md

Guidance for AI coding agents interacting with this repository. Two
audiences:

1. **Agents USING `aii` as a tool** (via MCP or CLI) to recall past
   conversations — most of this file.
2. **Agents WORKING ON `aii`'s code** — the last section.

If you're a human, read `README.md` instead.

---

## 1. Using `aii` as an agent

`aii` is a local SQLite FTS index over your user's Claude Code / Codex
/ Cursor transcripts. You can query it to recover context the user
expects you to "remember" — prior decisions, commands run, error
messages seen, implementations discussed.

### Preferred interface: MCP

If the user's client speaks MCP (Claude Code, Cursor, modern Codex),
`aii` runs as a registered MCP server and exposes these tools:

| Tool          | When to call                                                 |
|---------------|--------------------------------------------------------------|
| `search`      | Any "what did we...", "have I...", "how did I fix..." question |
| `get_session` | After a search, to read exact surrounding messages            |
| `related`     | "Find more threads on this topic" after locating one         |
| `stats`       | Sanity check; only if you suspect the index is empty         |

**The chain pattern** — this is the #1 thing to learn:

```
search("webhook retry")          → hit with cite "cc/32e869ac:319"
get_session("cc/32e869ac:319")   → messages around ordinal 319
```

When you pass a cite token as the `session` arg and omit
`around`/`from`/`to`, the tool anchors on that cite's ordinal
automatically with `span` context. This is almost always what you
want — the snippet in search results is truncated, but `get_session`
returns full message text.

### CLI fallback

If MCP isn't wired up, shell out to `aii` directly. When stdout is
piped, `aii` switches to ndjson automatically — you don't need a
flag.

```sh
aii search 'webhook retry' --limit 5
aii show cc/32e869ac:319 --span 3 --max-msg-chars 2000 --format ndjson
aii related 32e869ac --limit 5
aii help --json                      # schema of commands + flags
```

Each ndjson line is a self-contained object; you don't need to track
state across lines.

### Cite tokens

`aii` stamps every excerpt and every message with a stable cite token:

```
<agent_code>/<short_uid>:<ordinal>    e.g.  cc/32e869ac:319
```

- `agent_code` ∈ {`cc`, `cdx`, `cur`}
- `short_uid` = first 8 chars of the session UUID
- `ordinal` = 0-indexed position within the session

Regex: `^(cc|cdx|cur)/[0-9a-f]{8}:\d+$`

**When you cite a claim to the user, use this token.** It's
copy-pasteable, unambiguous, and round-trips to `get_session` /
`aii show`.

### Token budgets

Sessions can be huge (tool-result dumps often exceed 10k tokens each).
Be defensive:

- `max_msg_chars` (MCP) / `--max-msg-chars` (CLI) — truncate each
  message. `4000` is a good default; `2000` when you want a lot of
  messages in one call.
- `--max-bytes N` (CLI only) — cap total output. Truncation marker is
  emitted so the stream stays parseable.
- `span` — stay tight. `span=3` gets you 7 messages; `span=10` gets
  you 21 and often blows your context.

If you need more context after truncation, make a second call with
`from`/`to` for the specific range you need.

### Role filter

`role=user` returns **only what the user typed** — extremely
high-signal when you're trying to reconstruct "what was I trying to
do?" as opposed to "what did the previous agent say?". `role=assistant`
is useful for "what solution was proposed."

### Query idioms that work

- **Natural language** — `"webhook retry logic"` uses the porter
  tokenizer; handles stemming.
- **Code identifiers** — `"SessionUID"`, `"normalize_match"` — the
  trigram tokenizer finds these even though porter splits them.
- **Exact phrases** — `'"api key rotation"'` (note the inner double
  quotes).
- **Narrow scope** early: `agent=cc since=30d workspace=/path` before
  widening.

### Query idioms that DON'T work

- Boolean operators (`AND`, `OR`, `NOT`) — stripped by the normalizer
  to keep FTS5 safe. Use filter args instead.
- Regexes — not supported.
- Short tokens (< 3 chars) — the trigram branch is skipped; porter
  still runs as prefix-match.

### When to search vs. when to trust context

Search when any of these are true:
- The user says "remember", "last time", "what did we", "how did I".
- A fact is load-bearing and you don't have it in the current
  conversation.
- You're about to make a recommendation that references prior work.

Don't search when:
- The answer is already in the current conversation.
- It's a general programming question unrelated to this user's work.
- You'd just be showing off — each search costs time and tokens.

---

## 2. Working on `aii`'s code as an agent

### Build / test

```sh
go build ./...                          # everything
go build -o /tmp/aii ./cmd/aii          # binary
go vet ./...                            # static check
```

There's no test suite yet; validate with live smoke tests against
`~/.local/share/aii/aii.db`. Install to `~/.local/bin/aii` after
changes.

### Module layout

```
cmd/aii/                 # CLI. One package, main.go + color.go.
internal/source/         # source.Source interface + per-agent parsers.
internal/source/claudecode
internal/source/codex
internal/source/cursor
internal/indexer/        # orchestrates Sources → single writer goroutine
internal/store/          # sqlite schema, hybrid FTS search, migrations
internal/web/            # local HTTP UI + assets (embed.FS)
internal/tui/            # bubbletea two-pane UI
internal/mcpserver/      # stdio MCP server wrapping store
```

Dependencies kept minimal: `modernc.org/sqlite` (pure Go),
`charmbracelet/bubbletea+bubbles+lipgloss`, `mark3labs/mcp-go`,
`mattn/go-isatty` (indirect). **Don't add cgo.** The "one static
binary, zero network" promise is the whole design.

### Key invariants

- **Pure-Go SQLite.** `modernc.org/sqlite` only. No `mattn/go-sqlite3`,
  no `sqlite-vec`, no loadable extensions.
- **Single writer.** The indexer runs one goroutine writing to the DB;
  everything else is readers. Don't introduce concurrent writers.
- **External-content FTS gotcha.** `messages_fts` and
  `messages_fts_tri` are external-content tables. `COUNT(*)` delegates
  to `messages`, not the FTS shadow. Use `<table>_docsize` to detect
  empty indexes — see `migrateTrigram`.
- **Triggers must be rewired, not additive.** `CREATE TRIGGER IF NOT
  EXISTS` is a no-op when the trigger exists with a different body.
  The trigram migration drops and recreates all three message
  triggers.
- **modernc.org/sqlite planner quirks.** Hits CTE in search.go MUST
  stay `AS MATERIALIZED` — the planner otherwise tries to inline
  `bm25()`/`snippet()` into window functions and errors with
  "unable to use function bm25 in the requested context".

### Adding a new source

1. Drop a package under `internal/source/<name>/` implementing
   `source.Source`:
   ```go
   type Source interface {
       Name() string
       Collect(ctx context.Context, state StateReader, out chan<- Batch) error
   }
   ```
2. Register it in `cmd/aii/main.go#selectSources`.
3. Agent-code mapping (the 3-letter cite prefix): update `shortAgent`
   in `cmd/aii/main.go` and `ShortAgent` in
   `internal/mcpserver/helpers.go`. Update `agent_codes` in `help
   --json` and the cite-token regex.
4. Update `README.md` (source list) and this file.

### Agent-mode CLI changes

Any new command that emits data should:

- Accept `--json`, `--ndjson`, `--pretty`, `--agent-mode` and respect
  them via `pickSearchFormat` (or similar). Default to ndjson when
  `!stdoutIsTTY`.
- Emit cite tokens in all outputs (pretty + ndjson + json).
- Accept `--max-bytes` and truncate with a marker, not a hard cut.
- If sliceable, accept `--around/--span`, `--from/--to`, `--role`,
  `--max-msg-chars`.

### MCP changes

Tool descriptions are the tool's documentation — agents read them at
listing time. When adding or changing a tool:

- Explain **when** to call it, not just what it does.
- Reference the cite-token grammar if it takes a session ref.
- Note mutual exclusions between params (e.g. `around` vs `from`/`to`).
- Keep required params genuinely required. Default sane values for
  everything else.

Both `structuredContent` and a text fallback must be set. Use
`mcp.NewToolResultStructured(payload, fallback)`.

### What to avoid

- Don't break the cite-token format. It's referenced in tool
  descriptions and by user-facing docs; agents have been taught it.
- Don't add a network dependency to the default path. Optional is
  fine (`aii ask` shells to an LLM CLI), but `aii index`, `aii search`,
  and MCP tools must work fully offline.
- Don't remove `--dry-run` from `aii ask`. It's how the tool composes
  with everything else.
- Don't index more than FTS can handle. Per-message is fine
  (~160k messages runs smoothly); per-token would not be.
