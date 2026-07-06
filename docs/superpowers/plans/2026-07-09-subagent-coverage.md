# Subagent Transcript Coverage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Watch subagent transcript files (`<session>/subagents/**.jsonl`) so their token usage and cost are shipped, attributed to the parent session with an `agent_id`/`agent_type` envelope.

**Architecture:** The watcher's glob learns the two nested subagent layouts; the tail derives `(session_id, agent_id)` from the path (parent session ID for agent files) and lazily reads the `.meta.json` sidecar for `agent_type`. First-sighting fast-forward becomes mtime-gated (files born while we watch replay from zero — otherwise a subagent finishing within one poll interval would be skipped entirely). Timer synthetics are suppressed for agent files so a finished subagent never fires `session_idle`/`session_ended` under the parent's `session_id`.

**Tech Stack:** Go stdlib only (no new dependencies). Spec: `docs/superpowers/specs/2026-07-06-agentmon-design.md` (amended in commit 9f17182 — sections "Delivery semantics", "Watching", envelope example).

## Global Constraints

- No new dependencies. Go stdlib only (BurntSushi/toml already exists for config; nothing here touches config).
- Event identity is `(machine, session_id, agent_id, offset, seq)`; `agent_id` is `""` for main transcripts. Frozen seq sign convention untouched: parser events `seq ≥ 0`, watcher synthetics `seq ≤ -2`, `-1` reserved as fast-forward sentinel.
- New envelope fields `agent_id` and `agent_type` MUST be `omitempty` — main-transcript events must serialize byte-identically to events shipped before this change, or Loki's exact-duplicate dedupe stops absorbing replays of already-shipped history.
- Subagent events carry the **parent** session's ID (the path component above `subagents/`), never the agent file's own name, so per-session cost aggregations include the session's whole agent fleet.
- `description` and `spawnDepth` from `.meta.json` are never shipped — only `agentType`.
- Watcher timer synthetics (`session_idle`, `session_ended`, including the `removed` variant) never fire for agent files.
- First-sighting fast-forward applies only to files whose mtime is strictly before the watcher's start time (and never when `--backfill` is set). Files with mtime at-or-after watcher start replay from zero.
- All existing tests must stay green: `go build ./... && go test ./...`.
- macOS quirks: use `/bin/ls` not `ls` (aliased to eza); there is no `timeout` command.

---

### Task 1: Envelope fields + path identity + meta sidecar in the tail

**Files:**
- Modify: `internal/transcript/event.go` (Event struct, ~line 31-43)
- Modify: `internal/watch/tail.go`
- Test: `internal/watch/tail_test.go`

**Interfaces:**
- Consumes: existing `newTail(path string, mark state.Watermark, knownSize int64) *tail` and `(*tail).poll()`.
- Produces: `deriveIdentity(path string) (sessionID, agentID string)`; tail fields `agentID`, `agentType string` and `metaDone bool`; `(*tail).loadMeta()` called at the top of `poll()`. `transcript.Event` gains `AgentID`, `AgentType string` (both `omitempty`). Task 2 stamps `t.agentID`/`t.agentType` onto events in the watcher.

- [ ] **Step 1: Add envelope fields**

In `internal/transcript/event.go`, replace the Event struct comment and add two fields after `SessionID`:

```go
// Event is the envelope defined in the design spec. Identity for
// server-side dedupe is (machine, session_id, agent_id, offset, seq);
// Machine is stamped by the shipper, not the parser.
type Event struct {
	Machine   string `json:"machine,omitempty"`
	Project   string `json:"project,omitempty"`
	SessionID string `json:"session_id"`
	// AgentID/AgentType identify subagent transcripts; empty (and omitted
	// from JSON) for the session's main transcript. Omitempty is load-
	// bearing: main-transcript events must stay byte-identical to those
	// shipped before these fields existed, or Loki dedupe stops absorbing
	// replays.
	AgentID   string    `json:"agent_id,omitempty"`
	AgentType string    `json:"agent_type,omitempty"`
	Offset    int64     `json:"offset"`
	Seq       int       `json:"seq"`
	TS        time.Time `json:"ts,omitzero"`
	Type      EventType `json:"type"`
	Payload   Payload   `json:"payload"`
}
```

- [ ] **Step 2: Write the failing tests**

Append to `internal/watch/tail_test.go`:

```go
func TestDeriveIdentity(t *testing.T) {
	cases := []struct{ path, session, agent string }{
		{"/r/proj/abc-123.jsonl", "abc-123", ""},
		{"/r/proj/abc-123/subagents/agent-a1.jsonl", "abc-123", "agent-a1"},
		{"/r/proj/abc-123/subagents/workflows/wf_x-1/agent-a2.jsonl", "abc-123", "agent-a2"},
	}
	for _, c := range cases {
		s, a := deriveIdentity(c.path)
		if s != c.session || a != c.agent {
			t.Errorf("deriveIdentity(%q) = (%q,%q), want (%q,%q)", c.path, s, a, c.session, c.agent)
		}
	}
}

func TestAgentTailLoadsMetaLazily(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sess-1", "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent-a1.jsonl")
	write(t, path, line1)
	tl := newTail(path, state.Watermark{}, 0)
	if tl.sessionID != "sess-1" || tl.agentID != "agent-a1" {
		t.Fatalf("identity = (%q,%q), want (sess-1, agent-a1)", tl.sessionID, tl.agentID)
	}
	// Sidecar not written yet: poll must succeed and leave agentType empty.
	if _, _, err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	if tl.agentType != "" {
		t.Fatalf("agentType = %q before sidecar exists", tl.agentType)
	}
	// Sidecar appears later (Claude Code may write it after the transcript):
	// the next poll picks it up. description/spawnDepth are ignored.
	write(t, filepath.Join(dir, "agent-a1.meta.json"),
		`{"agentType":"general-purpose","description":"secret","spawnDepth":1}`)
	if _, _, err := tl.poll(); err != nil {
		t.Fatal(err)
	}
	if tl.agentType != "general-purpose" {
		t.Fatalf("agentType = %q, want general-purpose", tl.agentType)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/watch/ -run 'TestDeriveIdentity|TestAgentTailLoadsMetaLazily' -v`
Expected: FAIL with `undefined: deriveIdentity` (compile error).

- [ ] **Step 4: Implement in tail.go**

In `internal/watch/tail.go`, add `"encoding/json"` to the imports, add the new fields to the `tail` struct, replace `newTail`, add `deriveIdentity` and `loadMeta`, and call `loadMeta` at the top of `poll()`:

```go
type tail struct {
	path      string
	sessionID string
	agentID   string // "" for main transcripts
	agentType string // from the .meta.json sidecar, once read
	metaDone  bool   // sidecar read (or not applicable); stop retrying
	parser    *transcript.Parser
	readPos   int64 // bytes fed to parser (complete lines only)
	lastSize  int64 // size at previous poll, for activity detection
	mark      state.Watermark
	project   string
	synced    bool // parser state reflects file[0:readPos]
}

// deriveIdentity maps a transcript path to its session and agent identity.
// Main transcripts (<project>/<sid>.jsonl) have agentID "". Subagent
// transcripts live under a directory named after the parent session:
//
//	<project>/<sid>/subagents/agent-<id>.jsonl
//	<project>/<sid>/subagents/workflows/<wf>/agent-<id>.jsonl
//
// and take the parent session's ID plus their own agent ID, so per-session
// aggregations include the session's whole agent fleet.
func deriveIdentity(path string) (sessionID, agentID string) {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	parts := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
	for i := len(parts) - 1; i > 0; i-- {
		if parts[i] == "subagents" {
			return parts[i-1], base
		}
	}
	return base, ""
}

// newTail resumes a file at a persisted watermark. knownSize is the file
// size recorded when the watermark was saved: while the file still has
// exactly that size there is nothing new, so nothing is parsed; the
// first observed change triggers a replay from offset 0.
func newTail(path string, mark state.Watermark, knownSize int64) *tail {
	sessionID, agentID := deriveIdentity(path)
	return &tail{
		path:      path,
		sessionID: sessionID,
		agentID:   agentID,
		metaDone:  agentID == "", // main transcripts have no sidecar
		parser:    transcript.NewParser(sessionID),
		readPos:   knownSize,
		lastSize:  knownSize,
		mark:      mark,
		synced:    knownSize == 0,
	}
}

// loadMeta reads the agent file's .meta.json sidecar for agentType. The
// sidecar may appear after the transcript, so a missing file is retried
// on every poll until it is read once. description and spawnDepth are
// deliberately not shipped.
func (t *tail) loadMeta() {
	if t.metaDone {
		return
	}
	b, err := os.ReadFile(strings.TrimSuffix(t.path, ".jsonl") + ".meta.json")
	if err != nil {
		return
	}
	t.metaDone = true
	var m struct {
		AgentType string `json:"agentType"`
	}
	if json.Unmarshal(b, &m) == nil {
		t.agentType = m.AgentType
	}
}
```

At the very top of `poll()` (before the `os.Stat` call), add:

```go
	t.loadMeta()
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/watch/ ./internal/transcript/ -v`
Expected: PASS, including all pre-existing tests (existing `newTail` callers are unchanged; main-transcript behavior is identical because `deriveIdentity` returns the basename when no `subagents` component exists).

- [ ] **Step 6: Commit**

```bash
git add internal/transcript/event.go internal/watch/tail.go internal/watch/tail_test.go
git commit -m "feat: agent identity on the event envelope and tail (agent_id, agent_type)"
```

---

### Task 2: Watcher discovery, stamping, and synthetic suppression

**Files:**
- Modify: `internal/watch/watch.go` (`scan` ~line 188, `pollFile` emit loop ~line 108, `tickTimers` ~line 165, vanish loop in `PollOnce` ~line 59)
- Test: `internal/watch/watch_test.go`

**Interfaces:**
- Consumes: Task 1's `t.agentID`, `t.agentType` tail fields and `Event.AgentID`/`Event.AgentType`.
- Produces: `scan()` returning main + subagent + workflow-agent transcript paths; events from agent files stamped with `AgentID`/`AgentType`; no synthetics for agent files.

- [ ] **Step 1: Write the failing tests**

Append to `internal/watch/watch_test.go`:

```go
func TestSubagentFilesDiscoveredAndAttributed(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, _ := newTestWatcher(t, st, true)
	subDir := filepath.Join(dir, "sess-1", "subagents")
	wfDir := filepath.Join(subDir, "workflows", "wf_ab-1")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(subDir, "agent-a1.jsonl"), line1)
	write(t, filepath.Join(subDir, "agent-a1.meta.json"), `{"agentType":"general-purpose"}`)
	write(t, filepath.Join(wfDir, "agent-a2.jsonl"), line1)
	if err := w.PollOnce(); err != nil {
		t.Fatal(err)
	}
	var attributed []transcript.Event
	for _, ev := range c.events {
		if ev.SessionID == "sess-1" {
			attributed = append(attributed, ev)
		}
	}
	// 2 agent files × (session_started + user_prompt), all under the PARENT
	// session's ID.
	if len(attributed) != 4 {
		t.Fatalf("want 4 events attributed to sess-1, got %d (%v)", len(attributed), types(c.events))
	}
	agents := map[string]string{}
	for _, ev := range attributed {
		if ev.AgentID == "" {
			t.Errorf("subagent event missing agent_id: %+v", ev)
		}
		agents[ev.AgentID] = ev.AgentType
	}
	if agents["agent-a1"] != "general-purpose" {
		t.Errorf("agent-a1 agent_type = %q, want general-purpose", agents["agent-a1"])
	}
	if _, ok := agents["agent-a2"]; !ok {
		t.Errorf("workflow agent file not discovered; agents seen: %v", agents)
	}
}

func TestNoSyntheticsForSubagentFiles(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, now := newTestWatcher(t, st, true)
	subDir := filepath.Join(dir, "sess-1", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(subDir, "agent-a1.jsonl")
	write(t, path, line1+midTurnLine) // mid-turn: a main file would fire idle
	w.PollOnce()
	c.events = nil
	*now = now.Add(31 * time.Minute) // past IdleAfter AND EndedAfter
	w.PollOnce()
	if len(c.events) != 0 {
		t.Fatalf("quiet subagent file fired synthetics for the parent session: %v", types(c.events))
	}
	os.Remove(path)
	w.PollOnce()
	if len(c.events) != 0 {
		t.Fatalf("subagent removal fired synthetics: %v", types(c.events))
	}
	if _, ok := st.Files[path]; ok {
		t.Error("state entry not pruned after removal")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/watch/ -run 'TestSubagentFilesDiscoveredAndAttributed|TestNoSyntheticsForSubagentFiles' -v`
Expected: FAIL — `TestSubagentFilesDiscoveredAndAttributed` gets 0 attributed events (files not discovered); `TestNoSyntheticsForSubagentFiles` sees a `session_ended` synthetic.

- [ ] **Step 3: Implement in watch.go**

Replace `scan()`:

```go
func (w *Watcher) scan() []string {
	// Three transcript layouts (spec, "Watching"): the main session file
	// plus Task-tool and Workflow subagent files nested under a directory
	// named after the parent session.
	globs := []string{
		filepath.Join("*", "*.jsonl"),
		filepath.Join("*", "*", "subagents", "*.jsonl"),
		filepath.Join("*", "*", "subagents", "workflows", "*", "*.jsonl"),
	}
	var out []string
	for _, root := range w.opts.Roots {
		for _, g := range globs {
			matches, err := filepath.Glob(filepath.Join(root, g))
			if err != nil {
				continue
			}
			out = append(out, matches...)
		}
	}
	sort.Strings(out)
	return out
}
```

In `pollFile`, replace the emit loop:

```go
	for _, ev := range evs {
		ev.AgentID = t.agentID
		ev.AgentType = t.agentType
		w.emit(ev)
	}
```

At the top of `tickTimers`, add:

```go
	if t.agentID != "" {
		// Agent files share the parent's session_id: a subagent going
		// quiet must not report the interactive session idle or ended.
		// Session lifecycle belongs to the main transcript alone.
		return
	}
```

In `PollOnce`'s vanished-files loop, guard the synthetic (state still gets pruned):

```go
		if t, ok := w.tails[path]; ok {
			if !fs.Ended && t.agentID == "" {
				w.synthetic(t, fs, now, transcript.SessionEndedPayload{Reason: "removed"})
			}
			delete(w.tails, path)
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./...`
Expected: PASS across the whole module.

- [ ] **Step 5: Commit**

```bash
git add internal/watch/watch.go internal/watch/watch_test.go
git commit -m "feat: discover subagent transcripts; attribute to parent session; suppress agent synthetics"
```

---

### Task 3: mtime-gated first-sighting fast-forward

**Files:**
- Modify: `internal/watch/watch.go` (`Watcher` struct ~line 30, `New` ~line 40, `pollFile` first-sighting branch ~line 79-98)
- Test: `internal/watch/watch_test.go` (two existing tests updated, one new)

**Interfaces:**
- Consumes: `Options.Now` (already exists; tests drive a fake clock through it).
- Produces: `Watcher.startedAt time.Time` captured in `New` via `opts.Now()`. Fast-forward now requires `fi.ModTime().Before(w.startedAt)`.

**Why:** today ANY never-seen file is fast-forwarded — a subagent that starts and finishes within one poll interval is skipped entirely (its whole cost lost), and the first lines of every new main session are silently dropped. Files that predate the watcher must still fast-forward (no history flood on first run or after downtime).

- [ ] **Step 1: Update the two existing tests that rely on fast-forwarding recent files**

In `TestFirstSightingFastForwards` (watch_test.go ~line 43), the helper call currently discards the clock; capture it and pin the file into the past. Replace the opening lines:

```go
func TestFirstSightingFastForwards(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, now := newTestWatcher(t, st, false)
	path := filepath.Join(dir, "old.jsonl")
	write(t, path, line1+line2) // pre-existing history
	// Predates the watcher: this is the case fast-forward exists for.
	old := now.Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
```

(the rest of the test body is unchanged).

In `TestSyntheticAfterFastForwardDoesNotSwallowResume` (~line 264), the file must predate the watcher to be fast-forwarded, while staying recent enough that the +31min step still crosses EndedAfter. Replace the Chtimes block and its comment:

```go
	// Predates the watcher (so first sighting fast-forwards) but only
	// just: initTimers seeds activity from mtime, and 31 minutes later
	// the session must read as inactive. Anchored to the fake clock so
	// the test is deterministic regardless of when it runs.
	old := now.Add(-time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
```

- [ ] **Step 2: Write the new failing test**

Append to `internal/watch/watch_test.go`:

```go
func TestFileBornOnWatchReplaysFromZero(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, now := newTestWatcher(t, st, false) // non-backfill
	w.PollOnce()                                   // watcher running, root quiet
	// A file created while we watch (e.g. a subagent that starts and
	// finishes within one poll interval) must be captured in full, not
	// fast-forwarded past.
	path := filepath.Join(dir, "born.jsonl")
	write(t, path, line1)
	if err := os.Chtimes(path, *now, *now); err != nil {
		t.Fatal(err)
	}
	w.PollOnce()
	if got := types(c.events); len(got) != 2 || got[0] != transcript.SessionStarted {
		t.Fatalf("born-on-watch content skipped: %v", got)
	}
}
```

- [ ] **Step 3: Run tests to verify the new one fails and the updated ones pass**

Run: `go test ./internal/watch/ -run 'TestFileBornOnWatchReplaysFromZero|TestFirstSightingFastForwards|TestSyntheticAfterFastForward' -v`
Expected: `TestFileBornOnWatchReplaysFromZero` FAILS (fast-forward swallows the content, 0 events); the two updated tests PASS (they exercise current behavior, now with explicit past mtimes).

- [ ] **Step 4: Implement**

In `internal/watch/watch.go`, add the field and capture it in `New`:

```go
type Watcher struct {
	opts      Options
	startedAt time.Time
	st        *state.State
	sink      Sink
	tails     map[string]*tail

	FileErrs int // per-file stat/read errors (never fatal)
	SinkErrs int
}

func New(opts Options, st *state.State, sink Sink) *Watcher {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Watcher{opts: opts, startedAt: opts.Now(), st: st, sink: sink, tails: map[string]*tail{}}
}
```

In `pollFile`, replace the fast-forward condition and extend its comment:

```go
			// First sighting ever: fast-forward unless backfilling, so the
			// first run doesn't flood the sink with historical transcripts.
			// Only files that PREDATE this watcher fast-forward: a file
			// born while we watch (mtime at-or-after start) replays from
			// zero — otherwise a subagent that starts and finishes within
			// one poll interval would be skipped entirely.
			// A known file with an unset watermark (backfill mode, or a
			// shrink-reset saved mid-re-anchor) must NOT fast-forward: its
			// tail simply replays from zero on the next change, which at
			// worst re-emits already-shipped identities (at-least-once,
			// server dedupes) and never skips content.
			fi, err := os.Stat(path)
			if err != nil {
				w.FileErrs++
				w.st.Delete(path)
				return
			}
			if !w.opts.Backfill && fi.ModTime().Before(w.startedAt) {
				fs.Watermark = state.Watermark{Offset: fi.Size(), Seq: -1, Set: true}
				fs.Size = fi.Size()
			}
			w.initTimers(fs, fi, now)
```

- [ ] **Step 5: Run the full suite**

Run: `go build ./... && go test ./...`
Expected: PASS. (All other watch tests either use `backfill=true` — the gate is irrelevant — or now pin mtimes explicitly.)

- [ ] **Step 6: Commit**

```bash
git add internal/watch/watch.go internal/watch/watch_test.go
git commit -m "feat: replay files born while watching instead of fast-forwarding past them"
```

---

### Task 4: Grafana panel — cost by agent type

**Files:**
- Modify: `deploy/k8s/grafana/dashboards/agentmon.json`

**Interfaces:**
- Consumes: `agent_type` envelope field from Task 1 (flattened by LogQL `| json` as top-level `agent_type`; absent on main-session events → empty label value).

- [ ] **Step 1: Add the panel**

In `deploy/k8s/grafana/dashboards/agentmon.json`, append to the `panels` array (after panel id 10):

```json
{
  "id": 11,
  "type": "timeseries",
  "title": "Cost/hour by agent type (USD)",
  "description": "Blank agent_type = the interactive session's own messages; named types are subagent fleets (general-purpose, workflow-subagent, ...).",
  "gridPos": { "h": 7, "w": 18, "x": 6, "y": 32 },
  "datasource": { "type": "loki", "uid": "loki" },
  "fieldConfig": {
    "defaults": {
      "unit": "currencyUSD"
    }
  },
  "targets": [
    {
      "refId": "A",
      "expr": "sum by (agent_type) (sum_over_time({job=\"agentmon\", type=\"assistant_message\"} | json | unwrap payload_cost_usd | __error__=\"\" [$__auto]))"
    }
  ]
}
```

The grid position (`x:6, y:32, w:18, h:7`) fills the empty slot beside panel 10 (`x:0, y:32, w:6`).

- [ ] **Step 2: Validate JSON**

Run: `jq . deploy/k8s/grafana/dashboards/agentmon.json > /dev/null && echo OK`
Expected: `OK`

- [ ] **Step 3: Commit**

```bash
git add deploy/k8s/grafana/dashboards/agentmon.json
git commit -m "feat: cost-by-agent-type dashboard panel"
```

---

## Post-merge (human/controller, not a task)

- `kubectl apply -k deploy/k8s && kubectl -n agentmon rollout restart deploy/grafana` to pick up the dashboard configmap.
- Restart the watch daemon. Subagent files whose mtime predates the daemon start will be fast-forwarded (no historical flood); new subagent activity ships from then on. Historical subagent cost lands with the future time-sorted `agentmon backfill` (M4).
