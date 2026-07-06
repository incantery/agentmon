# agentmon

A dumb, reliable telemetry shipper for AI coding-agent sessions — plus the tiny
self-hosted server it ships to.

`agentmon watch` (coming) runs in the background on each of your machines,
tails Claude Code's session transcripts (`~/.claude/projects/**/*.jsonl`),
derives structured events, and pushes them to your own ingest server. The goal:
a single pane of glass across every machine you run coding agents on, and a
push alert when a session finishes or gets stuck — without any transcript
content leaving a machine you haven't allowed it to (per-machine
`metadata`/`full` content levels, enforced before anything touches disk).

Explicitly **not intelligent**: no LLM calls, no judgment, no orchestration.
Think `node_exporter` for agent sessions. Part of the
[incantery](https://github.com/incantery) suite of small, composable dev tools
for the AI-agent age.

## Status

Milestones 1–3 of 5 done: transcript parsing, live `watch` + spool, and the
LGTM sink (TOML config, Loki drain, `deploy/`). What works today:

```sh
make build
bin/agentmon parse --level metadata ~/.claude/projects/<project>/<session>.jsonl
```

`parse` streams a transcript as derived JSON events (session lifecycle, prompts,
tool calls/results, token usage, turn durations) — one JSON object per line —
and prints a summary of skipped line types to stderr. At `--level metadata`, no
prompt/file/tool content appears in the output; at `--level full`, content
fields are included (truncated to 2KB).

```sh
bin/agentmon watch --dry-run
```

`watch` polls `~/.claude/projects/**/*.jsonl` (default every 2s), derives the
same structured events as `parse`, stamps them with a machine name, and either
prints them (`--dry-run`, stdout sink + in-memory state — touches nothing on
disk) or appends them to an at-least-once disk spool plus a persisted state
file for resuming across restarts. Flags:

| flag | default | meaning |
| --- | --- | --- |
| `--roots` | `~/.claude/projects` | comma-separated transcript roots |
| `--machine` | `os.Hostname()` | machine name stamped on events |
| `--level` | `metadata` | `metadata` or `full` |
| `--interval` | `2s` | poll interval |
| `--idle-after` | `60s` | mid-turn inactivity before `session_idle` |
| `--ended-after` | `30m` | inactivity before `session_ended` |
| `--state-file` | `~/.local/state/agentmon/state.json` | watch state path |
| `--spool-dir` | `~/.local/state/agentmon/spool` | spool directory |
| `--spool-max-mb` | `256` | spool size cap (MB) |
| `--backfill` | off | emit pre-existing history on first sighting instead of fast-forwarding |
| `--dry-run` | off | print events to stdout; no state or spool writes |
| `--once` | off | poll once and exit |

When `[loki].url` is set (via `--config`, see below), `watch` drains the spool
to Loki every 10s; `agentmon drain --once` ships whatever's spooled manually,
without a `watch` running. `agentmon drain` refuses to run while a `watch` on
the same spool holds its lock (watch already drains that spool on its own).

## Config

Flags can also come from a per-machine TOML file (`--config`, default
`~/.config/agentmon/config.toml`) — flag defaults are seeded from it, and an
explicit flag still wins. Minimal example:

```toml
machine = "laptop"

[watch]
level = "metadata"

[loki]
url = "http://lab-host:3100"
```

See [`deploy/README.md`](deploy/README.md) for standing up the Loki + Grafana
backend this points at (Tilt + k8s, dashboard and ntfy alert included).

Coming next, per [the design](docs/superpowers/specs/2026-07-06-agentmon-design.md):
operational polish — launchd/systemd units, `agentmon emit` hook command.

## Design principles

- **Stdlib only** (so far), single binary, plain files.
- **Never crashes on transcript changes**: unknown line types, unknown content
  blocks, and malformed lines are counted and skipped, never fatal.
- **Redaction is structural**: the `metadata` level is enforced by a property
  test that walks every event payload type — a new payload can't silently leak.

## Development

```sh
go test ./...   # full suite (no network, no Chrome, no external deps)
make build      # → bin/agentmon
```

The transcript format is unversioned and observed, not documented — the parser
in `internal/transcript` treats missing fields as absent, not fatal, and the
verified field mappings live in
[the implementation plan](docs/superpowers/plans/2026-07-06-transcript-parser.md).
