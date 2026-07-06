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

Early — milestone 1 of 5. What works today:

```sh
make build
bin/agentmon parse --level metadata ~/.claude/projects/<project>/<session>.jsonl
```

`parse` streams a transcript as derived JSON events (session lifecycle, prompts,
tool calls/results, token usage, turn durations) — one JSON object per line —
and prints a summary of skipped line types to stderr. At `--level metadata`, no
prompt/file/tool content appears in the output; at `--level full`, content
fields are included (truncated to 2KB).

Coming next, per [the design](docs/superpowers/specs/2026-07-06-agentmon-design.md):
watch + spool (background tailing with at-least-once delivery), the ingest
server (SQLite + sessions API), alerts (ntfy), and service units.

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
