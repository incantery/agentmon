# agentmon — design

*2026-07-06 · incantery*

## What it is

`agentmon` is a **dumb, reliable telemetry shipper for AI coding-agent sessions**,
plus the tiny server it ships to. A background Go process on each machine watches
`~/.claude/projects/**/*.jsonl` (Claude Code session transcripts), derives events
from appended lines, and pushes them to a self-hosted ingest server on the home
lab. The server stores events, answers "what's happening across all my machines,"
and fires push alerts (session finished, session waiting).

Explicitly **not intelligent**: no LLM calls, no judgment, no orchestration. It is
`node_exporter`/`vector` for agent sessions. Intelligence (auto-triage, overseer
agents, dashboards) lives in *consumers* of the data, not here.

### Place in the incantery suite

Incantery is a composable development flow for the AI-agent age: small standalone
Go binaries that rendezvous on plain files and compose into one workflow. The
long-term ladder is (1) human orchestrates parallel interactive sessions with the
tools → (2) an interactive Claude Code session oversees other sessions through the
same files → (3) unattended workers move to Agent SDK + API billing. agentmon is
the **visibility substrate**: cross-machine, step-away-from-the-desk awareness of
what every session is doing. The later workspace/session-fabric tool reads the
same session state locally; agentmon is its remote transport.

## Goals

1. **Single pane of glass across machines** (work + personal): every Claude Code
   session's liveness, activity, and state, queryable in one place.
2. **Step-away alerts**: push notification when a session finishes a turn or
   appears to be waiting on input.
3. **Work-machine safe**: a per-machine content level controls whether transcript
   *content* ever leaves the machine.
4. **Zero per-project setup**: file watching means any session, started any way,
   in any repo, is covered.

### Non-goals (v1)

- No web UI (v2; the `sessions` CLI is the v1 pane of glass).
- No LLM calls, no auto-answering, no session control (read-only).
- No worktree/multiplexer awareness (that's the fabric tool, later).
- No Jira/Linear or other trackers.
- No non-Claude-Code agents — but the derived-event schema is agent-agnostic so a
  second `Source` implementation can be added without schema changes.

## Architecture

One repo, one binary, two roles:

```
work laptop                     home lab
┌─────────────────────┐        ┌──────────────────────────┐
│ agentmon watch      │  HTTP  │ agentmon serve           │
│  fswatch ~/.claude  │──────▶ │  ingest → SQLite         │
│  parse → derive     │ batch, │  alert rules → ntfy      │
│  redact (level)     │ token  │  /v1/sessions, /v1/events│
│  spool (disk)       │        └──────────────────────────┘
└─────────────────────┘                   ▲
personal laptop: same ──────────────────┘        agentmon sessions (CLI, anywhere)
```

- **`agentmon watch`** — per-machine shipper. launchd (macOS) / systemd (Linux)
  unit files provided. Pipeline: watch → tail from remembered offset → parse
  JSONL line → derive events → redact to level → append to spool → drain spool
  to server.
- **`agentmon serve`** — home-lab ingest. Batch ingest endpoint, SQLite storage,
  idempotent writes, alert evaluation, query API.
- **`agentmon sessions`** — CLI client of the query API (`--active`,
  `--machine`, `--json`). The v1 cross-machine view.
- **`agentmon tail <session>`** — CLI client streaming a session's stored events
  (respects what was shipped; at metadata level there is no content to show).

### Delivery semantics

At-least-once, server-side idempotent. Events carry identity
`(machine, session_id, offset, seq)` — `offset` is the byte offset of the source
line (timer-derived events reuse the last-seen offset) and `seq` is the index of
the event within that line's derivation, since one line can yield several events
(an `assistant` line → message + N tool calls). The server upserts on that key,
so replays
after crashes or retries are harmless. The spool is segmented JSONL on disk
(`~/.local/state/agentmon/spool/`); segments are deleted only after the server
acknowledges the batch. Offline (laptop asleep, VPN down, server rebooting) is a
non-event: the spool grows, then drains. Backpressure = spool size cap with
oldest-segment eviction and a `spool_evicted` marker event, so data loss is
visible, never silent.

### Watching

Transcript layout (verified against real files):
`~/.claude/projects/<encoded-project-path>/<session-uuid>.jsonl` — one file per
session, filename is the session ID, directory encodes the project path. Lines
are typed JSON objects; observed types include `user`, `assistant`, `system`,
`attachment`, `ai-title`, `mode`, `permission-mode`, `last-prompt`,
`file-history-snapshot`, `queue-operation`.

Watching is poll-based (default 2s): stat known files plus a glob rescan per
tick — no fsnotify dependency; latency is bounded by the interval, which is
fine for a monitor. First sighting of a file fast-forwards (no history
emitted) unless `--backfill` is set. Per-file state (inode, byte offset) is
persisted so restarts resume without re-shipping. Unknown line types are counted
and skipped — new Claude Code releases must never crash the shipper.

## Derived event schema

Envelope (every event):

```json
{
  "machine":    "seth-mbp-work",
  "project":    "/Users/sethlowie/go/src/github.com/incantery/sigil",
  "session_id": "d34f1644-…",
  "offset":     183422,
  "seq":        1,
  "ts":         "2026-07-06T14:03:22Z",
  "type":       "tool_call",
  "payload":    { }
}
```

Event types (v1):

| type | derived from | metadata-level payload | full-level adds |
|---|---|---|---|
| `session_started` | first line of a new file | cwd (often empty — first lines may carry none; envelope `project` backfills) | — |
| `session_title` | `ai-title` line | title | — |
| `user_prompt` | `user` line | char count | prompt text |
| `assistant_message` | `assistant` line | model, token usage, stop reason | content |
| `tool_call` | `assistant` tool_use | tool name | input (truncated) |
| `tool_result` | `user` tool_result | ok/error | content (truncated) |
| `permission_mode` | `permission-mode` line | mode | — |
| `turn_completed` | `system` line, subtype `turn_duration` | duration_ms, message count (the source line carries no token data; per-turn token totals live on `assistant_message` events) | — |
| `session_idle` | watcher timer: no writes mid-turn > threshold (default 60s) | idle seconds | — |
| `session_ended` | watcher timer: no writes > threshold (default 30min) or file removed | reason | — |
| `spool_evicted` | spool cap hit | dropped count | — |

**Leveling is enforced in the shipper, before spooling** — at `metadata` level,
content never exists off the transcript on that machine. Exact JSONL field
mappings (where token usage, stop reason, and tool blocks live inside `assistant`
lines) are pinned during implementation against fixture files; the transcript
format is unversioned and observed, so the parser must treat missing fields as
absent, not fatal.

**Known limitation:** a pending permission prompt is not reliably visible in the
JSONL (it may be written only after being answered), so "waiting for input"
starts life as the `session_idle` heuristic. The designed-for fix (not v1) is
`agentmon emit`, a hook command wired to Claude Code `Notification`/`Stop` hooks
that writes precise events straight into the spool, upgrading needs-attention
from heuristic to fact.

## Server

- **Storage:** SQLite (`modernc.org/sqlite`, CGO-free), two tables: `events`
  (envelope + JSON payload, unique on `(machine, session_id, offset, seq)`) and
  `sessions` (materialized latest-state per session: title, project, machine,
  state, last activity, token totals — updated on ingest).
- **API:** `POST /v1/events` (batch ingest), `GET /v1/sessions`,
  `GET /v1/sessions/{id}/events`. Bearer-token auth from config; the server is
  meant to sit on a private network (Tailscale/LAN) — the token is a seatbelt,
  not the security story. TLS optional/off by default accordingly.
- **Alerts:** rule evaluation on ingest; v1 rules are `turn_completed` and
  `session_idle`, filterable per machine/project, with per-session debounce
  (default 5min) so a chatty session doesn't spam. Sink: ntfy (or any
  webhook URL) from config.

## Config

Milestone 2 ships flags only (stdlib has no TOML); the config file lands with
`serve`.

TOML, one file per machine, `~/.config/agentmon/config.toml`:

```toml
machine = "seth-mbp-work"

[watch]
level = "metadata"          # or "full"
roots = ["~/.claude/projects"]
idle_after = "60s"
ended_after = "30m"
spool_max_mb = 256

[server]                     # used by watch (target) and CLI (query)
url = "http://homelab:7621"
token = "…"

[serve]                      # used by agentmon serve
listen = ":7621"
db = "/var/lib/agentmon/agentmon.db"
[serve.alerts]
ntfy_url = "https://ntfy.sh/…"
rules = ["turn_completed", "session_idle"]
debounce = "5m"
```

## Package layout

```
cmd/agentmon/        main; subcommands watch, serve, sessions, tail, emit (reserved)
internal/transcript/ JSONL line parsing + event derivation (pure; fixture-tested)
internal/redact/     level enforcement over derived events
internal/watch/      fs watching, offsets, idle/ended timers
internal/spool/      segmented disk spool, ack/evict
internal/ship/       batching HTTP client, retry/backoff
internal/serve/      HTTP server, ingest, query API
internal/store/      SQLite schema + queries, session materialization
internal/alert/      rules, debounce, ntfy/webhook sink
internal/config/     TOML loading + defaults
```

`internal/transcript` is the heart and is deliberately pure (lines in, events
out) — it is also the seam the future fabric tool imports to read session state
locally, and where a non-Claude `Source` would slot in.

## Testing

- **Golden fixtures:** sanitized real transcript files; `internal/transcript`
  tests assert exact derived-event streams, including unknown-type tolerance.
- **Redaction property:** at `metadata` level, no payload field may contain
  prompt/file text — enforced by a test that walks every event type's payload.
- **Pipeline integration:** temp dir, synthetic JSONL appends → `watch` →
  in-process `serve` → assert `sessions`/`events` API state; covers offsets,
  restart-resume, dedupe on replay.
- **Heuristics:** simulated clocks for idle/ended timers.
- **No network in tests**; ntfy sink tested against a local HTTP stub.

## Milestones (each ships working software)

1. **transcript** — parser + event derivation + leveling over fixtures. A
   `agentmon parse <file>` debug command proves it end to end.
2. **watch + spool** — live tailing with offsets and restart-resume; spool on
   disk; `agentmon watch --dry-run` prints derived events.
3. **serve + store** — ingest API, SQLite, session materialization; watch drains
   to it; `agentmon sessions` works. *(Single pane of glass exists here.)*
4. **alerts** — rules, debounce, ntfy sink. *(Step-away goal met here.)*
5. **operational polish** — launchd/systemd units, spool caps + eviction
   marker, `agentmon tail`, README.

## Future (designed-for, not built)

- `agentmon emit` hook command → precise needs-attention events.
- Web UI on the server (candidate sigil dogfood target).
- Second `Source` (Codex CLI or cantrip transcripts) behind the transcript seam.
- The fabric/overseer tool consuming `internal/transcript` locally.
