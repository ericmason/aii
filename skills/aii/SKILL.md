---
name: aii
description: Search and recall past AI coding conversations (Claude Code, Codex, Cursor) indexed locally by aii. Use when the user says "remember when", "last time", "what did we", "how did I fix", references prior decisions/commands/errors, or asks about past work that predates the current conversation. Also use proactively before making recommendations that build on prior work.
---

# aii — search past AI chats

`aii` is a local SQLite FTS index over the user's Claude Code, Codex, and
Cursor transcripts. Query it to recover context the user expects you to
"remember" — prior decisions, commands run, error messages seen,
implementations discussed.

Prefer the MCP tools (`search`, `list_sessions`, `get_session`,
`related`, `stats`) if they're registered. Otherwise shell out to the
`aii` CLI — when stdout is piped, `aii` emits ndjson automatically.

## When to search

Search when any of these is true:
- User uses recall language: "remember", "last time", "what did we",
  "how did I fix", "did I already…".
- A load-bearing fact isn't in the current conversation.
- You're about to recommend something that references prior work.

Don't search when:
- The answer is already in this conversation.
- It's a general programming question unrelated to the user's history.

## The chain pattern (most important)

```
search("webhook retry")          → hit with cite "cc/32e869ac:319"
get_session("cc/32e869ac:319")   → full messages around ordinal 319
```

Search snippets are truncated. When a hit looks promising, pass its
cite token to `get_session` (MCP) or `aii show` (CLI) to read the real
text. If the cite carries an ordinal and you omit `around`/`from`/`to`,
the ordinal is used as the anchor automatically with `span` context.

## Cite tokens

Every excerpt and message carries a stable token:

```
<agent_code>/<short_uid>:<ordinal>     e.g.  cc/32e869ac:319
```

- `agent_code` ∈ {`cc` = Claude Code, `cdx` = Codex, `cur` = Cursor}
- `short_uid` = first 8 chars of the session UUID
- `ordinal` = 0-indexed position within the session
- Regex: `^(cc|cdx|cur)/[0-9a-f]{8}:\d+$`

**When you cite a claim back to the user, use this token.** It's
copy-pasteable and round-trips to `get_session` / `aii show`.

## MCP tools (preferred)

| Tool             | When to call                                                    |
|------------------|-----------------------------------------------------------------|
| `search`         | Any "what did we…", "have I…", "how did I fix…" question        |
| `list_sessions`  | "What was I working on last week?" — browse a time window with no full-text query |
| `get_session`    | After a search/list, to read exact surrounding messages         |
| `related`        | "Find more threads on this topic" after locating one            |
| `stats`          | Sanity check; only if you suspect the index is empty            |

`search(query, agent?, workspace?, role?, since?, limit?, offset?)`
`list_sessions(agent?, workspace?, since?, until?, limit?, offset?, order?, ended_mid_task?)`
`get_session(session, around?, span?, from?, to?, role?, max_msg_chars?)`
`related(session, limit?, agent?, since?)`

`list_sessions` with `ended_mid_task=true` returns sessions whose
final message was from the user or a tool — i.e. interrupted threads
where the assistant never responded. Useful for "what did I leave
unfinished?" questions.

## CLI fallback

```sh
aii search 'webhook retry' --limit 5 --max-bytes 8000
aii sessions --since 7d --agent cc                    # browse a time window
aii sessions --since 7d --ended-mid-task              # only interrupted threads
aii show cc/32e869ac:319 --span 3 --max-msg-chars 2000 --format ndjson
aii related 32e869ac --limit 5
aii help --json                      # schema of commands + flags
```

Each ndjson line is self-contained — no cross-line state to track.

## Query idioms that work

- **Natural language** — `"webhook retry logic"` (porter tokenizer,
  handles stemming).
- **Code identifiers** — `"SessionUID"`, `"normalize_match"` (trigram
  tokenizer finds these even though porter splits on case/underscore).
- **Exact phrases** — `'"api key rotation"'` (inner double quotes).
- **Narrow scope early** — `agent=cc since=30d workspace=/path`, then
  widen only if empty.

## Query idioms that DON'T work

- Boolean operators (`AND`, `OR`, `NOT`) — stripped by the normalizer
  to keep FTS5 safe. Use filter args instead.
- Regexes — not supported.
- Short tokens (< 3 chars) — the trigram branch is skipped; porter
  still runs as prefix-match.

## Role filter

- `role=user` — **only what the user typed.** High-signal when
  reconstructing "what was I trying to do?" vs. "what did the previous
  agent say?".
- `role=assistant` — proposed solutions and explanations.

## Token budgets — be defensive

Sessions can be huge (tool-result dumps often exceed 10k tokens each).

- `max_msg_chars` (MCP) / `--max-msg-chars` (CLI) — truncate each
  message. `4000` is a sane default; `2000` when you want many
  messages in one call.
- `--max-bytes N` (CLI) — cap total output. A truncation marker is
  emitted so the stream stays parseable.
- `span` — stay tight. `span=3` returns 7 messages; `span=10` returns
  21 and often blows the context window.

If you still need more after truncation, make a second call with
`from`/`to` for just the range you need.

## Common patterns

### Reconstruct what the user was trying to do

```sh
aii search 'flakey tests' --workspace "$HOME/code/myproject" --role user --limit 5
```

Then expand the best hit:

```sh
aii show cc/abc12345:42 --span 5 --role user --max-msg-chars 2000
```

### Find the fix for a recurring issue

```sh
aii search '"connection refused" redis' --since 30d --limit 10
```

Scan hits for one that looks like the resolution, then pivot:

```sh
aii related <uid> --limit 5
```

### Verify the index is populated

```sh
aii stats
aii doctor       # DB path, source paths, per-agent counts
```

## Citing back to the user

Format: `Last time you hit this (cc/32e869ac:319), you …`. The user
can paste that token into `aii show` to jump straight to the moment.
Don't paraphrase the cite — emit it verbatim.
