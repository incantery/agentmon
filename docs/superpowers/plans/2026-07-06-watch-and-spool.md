# agentmon Milestone 2: Watch + Spool Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `agentmon watch` — a poll-based background tailer that turns live Claude Code transcripts into derived events, with restart-safe identity (replay-from-zero + ship watermark), idle/ended session timers, a segmented disk spool, and a `--dry-run` mode that prints events to stdout.

**Architecture:** A `tail` per transcript file feeds the milestone-1 parser and filters against a persisted per-file watermark; a `Watcher` polls all files on a fixed interval (no fsnotify — approved decision), stamps machine identity, applies redaction, and emits to a `Sink` (spool or stdout). All timer logic runs on an injected clock so every behavior is unit-testable via `PollOnce()` without goroutines or sleeps.

**Tech Stack:** Go 1.25, stdlib only. Builds on `internal/transcript` (unchanged parser) and `internal/redact`.

## Global Constraints

- stdlib only; module `github.com/incantery/agentmon`; go 1.25.
- The milestone-1 parser (`transcript.NewParser`/`Line`) is consumed **unchanged**. A parser must observe its file from offset 0 (documented contract); resume correctness comes from replay-from-zero + watermark filtering, never parser state snapshots.
- Event identity is `(machine, session_id, offset, seq)`. Watcher-synthesized events (session_idle, session_ended, spool_evicted) continue the `seq` counter at the file's last offset so identities never collide.
- Redaction (`redact.Apply`) happens in the watcher **before** any event reaches a sink; at `metadata` level no content is ever spooled.
- Timer defaults from the spec: `session_idle` after **60s** with no writes mid-turn; `session_ended` after **30m** with no writes. Poll interval default **2s**. Spool cap default **256 MB**, segment size **4 MB**.
- Unknown/hostile input never panics; per-file errors are counted, not fatal.
- **Approved deviation from spec:** config is CLI flags for this milestone (stdlib has no TOML; the config file arrives with `serve`). **Approved decision:** poll-only watching (stat + glob per tick), no fsnotify dependency.
- Every task ends with `go build ./... && go test ./...` green and a commit.

## Resume semantics (the core invariant — read before Task 4)

- **Watermark** = identity `(offset, seq)` of the last event emitted for a file, plus a `Set` flag (`Set:false` = nothing emitted; a zero-value `{0,0}` watermark would wrongly cover the first event at offset 0/seq 0).
- **Restart:** a tail is created with the persisted watermark and `knownSize` (file size at last save). If the file is still exactly `knownSize`, nothing is parsed. On the first observed change, the tail replays from offset 0 through a fresh parser and the watermark drops everything already emitted — identity is deterministic because the parser always sees the file from 0.
- **First sighting** (no prior state): the watcher **fast-forwards** — it records watermark `{Offset: size, Seq: -1, Set: true}` and parses nothing, so the first run doesn't flood the spool with months of history. Everything appended afterwards is emitted. `--backfill` disables fast-forward (emits full history; used to seed the server later).
- **Shrink/rewrite** (`size < readPos`): prior offsets are invalid — clear the watermark, replay from 0 with fresh identity space.

## File structure

```
internal/transcript/event.go        MODIFY: +3 watcher event types
internal/redact/redact_test.go      MODIFY: +Reason to allowlist
internal/state/state.go             NEW: Watermark, FileState, State (JSON, atomic save)
internal/spool/spool.go             NEW: segmented spool, rotation, cap eviction
internal/watch/tail.go              NEW: per-file tailing + watermark filter
internal/watch/watch.go             NEW: Watcher: scan, PollOnce, timers, Run
cmd/agentmon/watch.go               NEW: watch subcommand, sinks, flags
cmd/agentmon/main.go                MODIFY: register watch subcommand
```

---

### Task 1: Watcher event types

**Files:**
- Modify: `internal/transcript/event.go`
- Modify: `internal/transcript/event_test.go`
- Modify: `internal/redact/redact_test.go`

**Interfaces:**
- Consumes: existing `EventType`, `Payload`, `AllEventTypes`, `AllPayloads`.
- Produces: `SessionIdle`, `SessionEnded`, `SpoolEvicted` EventType consts; `SessionIdlePayload{IdleSeconds int64}`, `SessionEndedPayload{Reason string}`, `SpoolEvictedPayload{Dropped int}` — all registered in `AllEventTypes`/`AllPayloads`.

- [ ] **Step 1: Write the failing test**

Append to `internal/transcript/event_test.go`:

```go
func TestWatcherEventTypesRegistered(t *testing.T) {
	for _, et := range []EventType{SessionIdle, SessionEnded, SpoolEvicted} {
		found := false
		for _, x := range AllEventTypes {
			if x == et {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from AllEventTypes", et)
		}
	}
	b, err := json.Marshal(SessionEndedPayload{Reason: "inactive"})
	if err != nil || string(b) != `{"reason":"inactive"}` {
		t.Errorf("SessionEndedPayload JSON = %s, %v", b, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transcript/ -run TestWatcherEventTypes`
Expected: FAIL — `undefined: SessionIdle`.

- [ ] **Step 3: Implement**

In `internal/transcript/event.go`: add to the const block

```go
	// Watcher-produced (milestone 2): not derived from transcript lines.
	SessionIdle  EventType = "session_idle"
	SessionEnded EventType = "session_ended"
	SpoolEvicted EventType = "spool_evicted"
```

append the three types to `AllEventTypes`, add the payload structs

```go
type SessionIdlePayload struct {
	IdleSeconds int64 `json:"idle_seconds"`
}

type SessionEndedPayload struct {
	Reason string `json:"reason"` // "inactive" | "removed"
}

type SpoolEvictedPayload struct {
	Dropped int `json:"dropped"`
}

func (SessionIdlePayload) EventType() EventType  { return SessionIdle }
func (SessionEndedPayload) EventType() EventType { return SessionEnded }
func (SpoolEvictedPayload) EventType() EventType { return SpoolEvicted }
```

and register all three zero values in `AllPayloads()`.

In `internal/redact/redact_test.go`, add `"Reason": true` to `allowedStrings` (the values are the fixed enums "inactive"/"removed", not content) — the property test fails without it, which is it doing its job.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./...`
Expected: PASS everywhere, including `TestMetadataClearsAllContentFields` and `TestEveryEventTypeHasExactlyOnePayload` over the grown registry.

- [ ] **Step 5: Commit**

```bash
git add internal/transcript/ internal/redact/
git commit -m "feat(transcript): watcher event types — session_idle, session_ended, spool_evicted"
```

---

### Task 2: `internal/state` — watermarks and persisted file state

**Files:**
- Create: `internal/state/state.go`
- Test: `internal/state/state_test.go`

**Interfaces:**
- Consumes: nothing (stdlib only).
- Produces:
  - `type Watermark struct { Offset int64; Seq int; Set bool }` (JSON tags `offset`, `seq`, `set`) with `func (w Watermark) Covers(offset int64, seq int) bool`.
  - `type FileState struct { Watermark Watermark; Size int64; LastActivityUnix int64; MidTurn bool; IdleFired bool; Ended bool }` (JSON tags `watermark`, `size`, `last_activity_unix`, `mid_turn`, `idle_fired`, `ended`).
  - `func Load(path string) (*State, error)` — missing file → empty state; `path == ""` → in-memory (Save is a no-op).
  - `(*State).File(key string) *FileState` (get-or-create), `(*State).Delete(key string)`, `(*State).Save() error` (atomic: temp file in same dir + rename), exported field `Files map[string]*FileState`.

- [ ] **Step 1: Write the failing tests**

`internal/state/state_test.go`:

```go
package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWatermarkCovers(t *testing.T) {
	empty := Watermark{}
	if empty.Covers(0, 0) {
		t.Error("unset watermark must cover nothing (the offset-0/seq-0 first event)")
	}
	w := Watermark{Offset: 100, Seq: 2, Set: true}
	cases := []struct {
		off  int64
		seq  int
		want bool
	}{
		{50, 9, true},   // earlier line
		{100, 1, true},  // same line, earlier seq
		{100, 2, true},  // exactly the watermark
		{100, 3, false}, // same line, later seq
		{150, 0, false}, // later line
	}
	for _, c := range cases {
		if got := w.Covers(c.off, c.seq); got != c.want {
			t.Errorf("Covers(%d,%d) = %v, want %v", c.off, c.seq, got, c.want)
		}
	}
	// fast-forward form: {Offset: size, Seq: -1} covers every line below size
	ff := Watermark{Offset: 500, Seq: -1, Set: true}
	if !ff.Covers(499, 7) || ff.Covers(500, 0) {
		t.Error("fast-forward watermark semantics wrong")
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || s == nil || len(s.Files) != 0 {
		t.Fatalf("Load missing: %v %v", s, err)
	}
}

func TestRoundtripAndDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Load(path)
	fs := s.File("/a/b.jsonl")
	fs.Watermark = Watermark{Offset: 42, Seq: 1, Set: true}
	fs.Size = 100
	fs.MidTurn = true
	s.File("/gone.jsonl")
	s.Delete("/gone.jsonl")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.File("/a/b.jsonl")
	if got.Watermark != (Watermark{Offset: 42, Seq: 1, Set: true}) || got.Size != 100 || !got.MidTurn {
		t.Errorf("roundtrip lost data: %+v", got)
	}
	if _, ok := s2.Files["/gone.jsonl"]; ok {
		t.Error("Delete did not persist")
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestInMemorySaveIsNoop(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	s.File("/x").Size = 1
	if err := s.Save(); err != nil {
		t.Fatalf("in-memory Save must be a no-op, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/state/`
Expected: FAIL — package doesn't exist / `undefined: Watermark`.

- [ ] **Step 3: Implement**

`internal/state/state.go`:

```go
// Package state persists per-file watch progress: the ship watermark
// (identity of the last emitted event) plus the timer bookkeeping the
// watcher needs across restarts.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Watermark is the identity of the last event emitted for a file.
// The zero value (Set == false) covers nothing — required so the very
// first event of a file (offset 0, seq 0) is not swallowed.
// The fast-forward form {Offset: size, Seq: -1, Set: true} covers every
// line starting below size and nothing at or after it.
type Watermark struct {
	Offset int64 `json:"offset"`
	Seq    int   `json:"seq"`
	Set    bool  `json:"set"`
}

// Covers reports whether the event identified by (offset, seq) was
// already emitted at or before this watermark.
func (w Watermark) Covers(offset int64, seq int) bool {
	if !w.Set {
		return false
	}
	if offset != w.Offset {
		return offset < w.Offset
	}
	return seq <= w.Seq
}

type FileState struct {
	Watermark        Watermark `json:"watermark"`
	Size             int64     `json:"size"`
	LastActivityUnix int64     `json:"last_activity_unix"`
	MidTurn          bool      `json:"mid_turn"`
	IdleFired        bool      `json:"idle_fired"`
	Ended            bool      `json:"ended"`
}

type State struct {
	path  string
	Files map[string]*FileState `json:"files"`
}

// Load reads the state file at path; a missing file yields an empty
// state. path == "" yields an in-memory state whose Save is a no-op
// (used by --dry-run).
func Load(path string) (*State, error) {
	s := &State{path: path, Files: map[string]*FileState{}}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	if s.Files == nil {
		s.Files = map[string]*FileState{}
	}
	return s, nil
}

// File returns the state entry for key, creating it if absent.
func (s *State) File(key string) *FileState {
	fs, ok := s.Files[key]
	if !ok {
		fs = &FileState{}
		s.Files[key] = fs
	}
	return fs
}

func (s *State) Delete(key string) { delete(s.Files, key) }

// Save writes the state atomically (temp file + rename).
func (s *State) Save() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/state/`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/state/
git commit -m "feat(state): watermarks + persisted per-file watch state with atomic save"
```

---

### Task 3: `internal/spool` — segmented disk spool

**Files:**
- Create: `internal/spool/spool.go`
- Test: `internal/spool/spool_test.go`

**Interfaces:**
- Consumes: nothing (stdlib only).
- Produces:
  - `func Open(dir string, segMaxBytes, totalMaxBytes int64) (*Spool, error)` — creates dir, continues segment numbering after any existing `spool-NNNNNNNN.jsonl`, always starts a fresh segment.
  - `(*Spool).Append(line []byte) (evicted int, err error)` — writes `line` + `\n` to the current segment, rotates when the segment reaches segMaxBytes, then evicts oldest **closed** segments while total size exceeds totalMaxBytes; returns the number of event lines dropped by eviction.
  - `(*Spool).Segments() ([]string, error)` (sorted paths), `(*Spool).Close() error`.

- [ ] **Step 1: Write the failing tests**

`internal/spool/spool_test.go`:

```go
package spool

import (
	"bytes"
	"os"
	"testing"
)

func TestAppendWritesLine(t *testing.T) {
	sp, err := Open(t.TempDir(), 1<<20, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	if _, err := sp.Append([]byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	segs, _ := sp.Segments()
	if len(segs) != 1 {
		t.Fatalf("segments = %v", segs)
	}
	data, _ := os.ReadFile(segs[0])
	if string(data) != "{\"a\":1}\n" {
		t.Errorf("segment content %q", data)
	}
}

func TestRotationAndReopenNumbering(t *testing.T) {
	dir := t.TempDir()
	sp, _ := Open(dir, 10, 1<<30) // tiny segments: every line rotates
	sp.Append([]byte("0123456789"))
	sp.Append([]byte("0123456789"))
	sp.Close()
	sp2, _ := Open(dir, 10, 1<<30)
	defer sp2.Close()
	sp2.Append([]byte("x"))
	segs, _ := sp2.Segments()
	if len(segs) != 3 {
		t.Fatalf("want 3 segments after reopen, got %v", segs)
	}
	for i := 1; i < len(segs); i++ {
		if segs[i] <= segs[i-1] {
			t.Errorf("segment names not increasing: %v", segs)
		}
	}
}

func TestEvictionCountsLinesAndSparesCurrent(t *testing.T) {
	dir := t.TempDir()
	// segMax 30 → ~2 lines per segment; totalMax 60 → keep ~2 segments
	sp, _ := Open(dir, 30, 60)
	defer sp.Close()
	var evicted int
	for i := 0; i < 12; i++ {
		n, err := sp.Append([]byte(`{"line":` + string(rune('0'+i%10)) + `}`)) // 10-11 bytes
		if err != nil {
			t.Fatal(err)
		}
		evicted += n
	}
	if evicted == 0 {
		t.Fatal("expected evictions with a 60-byte cap")
	}
	segs, _ := sp.Segments()
	if len(segs) == 0 {
		t.Fatal("current segment must never be evicted")
	}
	var total int64
	var lines int
	for _, s := range segs {
		data, _ := os.ReadFile(s)
		total += int64(len(data))
		lines += bytes.Count(data, []byte("\n"))
	}
	if lines+evicted != 12 {
		t.Errorf("lines kept (%d) + evicted (%d) != 12 appended", lines, evicted)
	}
	if len(segs) > 1 && total > 60+30 {
		t.Errorf("total size %d far exceeds cap", total)
	}
}

func TestOnlyCurrentSegmentNeverEvicted(t *testing.T) {
	sp, _ := Open(t.TempDir(), 1<<20, 1) // cap smaller than any line
	defer sp.Close()
	n, err := sp.Append([]byte("survives"))
	if err != nil || n != 0 {
		t.Fatalf("current segment was evicted: n=%d err=%v", n, err)
	}
	segs, _ := sp.Segments()
	if len(segs) != 1 {
		t.Fatalf("segments = %v", segs)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/spool/`
Expected: FAIL — `undefined: Open`.

- [ ] **Step 3: Implement**

`internal/spool/spool.go`:

```go
// Package spool is agentmon's at-least-once disk buffer: events are
// appended as JSON lines to size-rotated segment files; milestone 3's
// shipper deletes segments after server acknowledgment. When the total
// size exceeds the cap, oldest CLOSED segments are evicted (the loss is
// reported so the watcher can emit a spool_evicted marker — never
// silent). The current segment is never evicted.
package spool

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Spool struct {
	dir      string
	segMax   int64
	totalMax int64
	cur      *os.File
	curSize  int64
	curIndex int
}

func Open(dir string, segMaxBytes, totalMaxBytes int64) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Spool{dir: dir, segMax: segMaxBytes, totalMax: totalMaxBytes}
	segs, err := s.Segments()
	if err != nil {
		return nil, err
	}
	if len(segs) > 0 {
		last := filepath.Base(segs[len(segs)-1])
		fmt.Sscanf(last, "spool-%08d.jsonl", &s.curIndex)
	}
	if err := s.rotate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Spool) rotate() error {
	if s.cur != nil {
		if err := s.cur.Close(); err != nil {
			return err
		}
	}
	s.curIndex++
	name := filepath.Join(s.dir, fmt.Sprintf("spool-%08d.jsonl", s.curIndex))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.cur = f
	s.curSize = 0
	return nil
}

// Append writes one event line and enforces rotation + the total-size
// cap. It returns how many previously spooled event lines were dropped
// to stay under the cap (0 in the normal case).
func (s *Spool) Append(line []byte) (evicted int, err error) {
	if s.curSize >= s.segMax {
		if err := s.rotate(); err != nil {
			return 0, err
		}
	}
	if _, err := s.cur.Write(line); err != nil {
		return 0, err
	}
	if _, err := s.cur.Write([]byte{'\n'}); err != nil {
		return 0, err
	}
	s.curSize += int64(len(line)) + 1
	return s.evict()
}

func (s *Spool) evict() (int, error) {
	dropped := 0
	for {
		segs, err := s.Segments()
		if err != nil {
			return dropped, err
		}
		if len(segs) <= 1 {
			return dropped, nil // never evict the current segment
		}
		var total int64
		for _, p := range segs {
			if fi, err := os.Stat(p); err == nil {
				total += fi.Size()
			}
		}
		if total <= s.totalMax {
			return dropped, nil
		}
		oldest := segs[0]
		data, err := os.ReadFile(oldest)
		if err != nil {
			return dropped, err
		}
		if err := os.Remove(oldest); err != nil {
			return dropped, err
		}
		dropped += bytes.Count(data, []byte{'\n'})
	}
}

// Segments returns all segment paths, oldest first (current segment last).
func (s *Spool) Segments() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "spool-") && strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, filepath.Join(s.dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Spool) Close() error {
	if s.cur == nil {
		return nil
	}
	return s.cur.Close()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/spool/`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/spool/
git commit -m "feat(spool): segmented disk spool with rotation and audited cap eviction"
```

---

### Task 4: `internal/watch` tail — replay-from-zero with watermark filter

**Files:**
- Create: `internal/watch/tail.go`
- Test: `internal/watch/tail_test.go`

**Interfaces:**
- Consumes: `transcript.NewParser`, `(*transcript.Parser).Line`, `transcript.Event`; `state.Watermark.Covers`.
- Produces (package-internal, used by Task 5's Watcher):
  - `func newTail(path string, mark state.Watermark, knownSize int64) *tail` — sessionID = filename stem.
  - `(*tail).poll() (events []transcript.Event, grew bool, err error)` — reads new complete lines, returns only events not covered by the watermark; `grew` = file size changed since last poll (activity signal).
  - Fields read by the Watcher: `t.mark state.Watermark` (updated to last emitted identity), `t.project string` (last seen project), `t.sessionID string`, `t.lastSize int64`.

**Semantics (from the plan header, restated):** created with `knownSize` and `synced=false`; nothing is parsed until the size changes, then a full replay from 0 through a fresh parser with the watermark dropping already-emitted events. `size < readPos` (rewrite) clears the watermark and replays with fresh identity. A trailing line without `\n` is left unconsumed (it may be mid-write).

- [ ] **Step 1: Write the failing tests**

`internal/watch/tail_test.go`:

```go
package watch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/incantery/agentmon/internal/state"
	"github.com/incantery/agentmon/internal/transcript"
)

const line1 = `{"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2026-07-06T10:00:00.000Z","cwd":"/p","sessionId":"s1"}` + "\n"
const line2 = `{"type":"ai-title","aiTitle":"T","sessionId":"s1"}` + "\n"

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func types(evs []transcript.Event) []transcript.EventType {
	var out []transcript.EventType
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

func TestFreshFileEmitsEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	write(t, path, line1)
	tl := newTail(path, state.Watermark{}, 0)
	evs, grew, err := tl.poll()
	if err != nil || !grew {
		t.Fatalf("grew=%v err=%v", grew, err)
	}
	// session_started + user_prompt
	if got := types(evs); len(got) != 2 || got[0] != transcript.SessionStarted || got[1] != transcript.UserPrompt {
		t.Fatalf("types = %v", got)
	}
	if tl.project != "/p" || tl.sessionID != "s1" {
		t.Errorf("project=%q session=%q", tl.project, tl.sessionID)
	}
	if !tl.mark.Set {
		t.Error("watermark not advanced")
	}
}

func TestResumeUnchangedParsesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	write(t, path, line1)
	fi, _ := os.Stat(path)
	tl := newTail(path, state.Watermark{Offset: 0, Seq: 1, Set: true}, fi.Size())
	evs, grew, err := tl.poll()
	if err != nil || grew || len(evs) != 0 {
		t.Fatalf("unchanged resume: evs=%v grew=%v err=%v", evs, grew, err)
	}
}

func TestResumeThenAppendReplaysWithoutDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	write(t, path, line1)
	fi, _ := os.Stat(path)
	// watermark says line1's two events (seq 0,1 at offset 0) were emitted
	tl := newTail(path, state.Watermark{Offset: 0, Seq: 1, Set: true}, fi.Size())
	appendTo(t, path, line2)
	evs, grew, err := tl.poll()
	if err != nil || !grew {
		t.Fatalf("grew=%v err=%v", grew, err)
	}
	// replay saw line1 again but the watermark drops it; only session_title emits
	if got := types(evs); len(got) != 1 || got[0] != transcript.SessionTitle {
		t.Fatalf("types = %v (duplicates leaked or new event lost)", got)
	}
	if evs[0].Offset != int64(len(line1)) || evs[0].Seq != 0 {
		t.Errorf("identity = (%d,%d), want (%d,0)", evs[0].Offset, evs[0].Seq, len(line1))
	}
}

func TestShrinkResetsIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	write(t, path, line1+line2)
	tl := newTail(path, state.Watermark{}, 0)
	tl.poll()
	write(t, path, line2) // rewritten smaller
	evs, _, err := tl.poll()
	if err != nil {
		t.Fatal(err)
	}
	// fresh identity space: session_started + session_title from offset 0
	if got := types(evs); len(got) != 2 || got[0] != transcript.SessionStarted {
		t.Fatalf("types after rewrite = %v", got)
	}
	if evs[0].Offset != 0 {
		t.Errorf("rewrite must restart offsets at 0, got %d", evs[0].Offset)
	}
}

func TestPartialLineLeftForNextPoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	write(t, path, line1+`{"type":"ai-ti`) // torn mid-write
	tl := newTail(path, state.Watermark{}, 0)
	evs, _, _ := tl.poll()
	if got := types(evs); len(got) != 2 {
		t.Fatalf("partial line must not parse: %v", got)
	}
	appendTo(t, path, `tle","aiTitle":"T","sessionId":"s1"}`+"\n")
	evs2, _, _ := tl.poll()
	if got := types(evs2); len(got) != 1 || got[0] != transcript.SessionTitle {
		t.Fatalf("completed line lost: %v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/watch/`
Expected: FAIL — `undefined: newTail`.

- [ ] **Step 3: Implement**

`internal/watch/tail.go`:

```go
// Package watch tails Claude Code transcript files and turns appended
// lines into derived events with restart-safe identity: a tail always
// feeds its parser from offset 0 (the parser contract) and a persisted
// watermark drops events that were already emitted in a prior run.
package watch

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/incantery/agentmon/internal/state"
	"github.com/incantery/agentmon/internal/transcript"
)

type tail struct {
	path      string
	sessionID string
	parser    *transcript.Parser
	readPos   int64 // bytes fed to parser (complete lines only)
	lastSize  int64 // size at previous poll, for activity detection
	mark      state.Watermark
	project   string
	synced    bool // parser state reflects file[0:readPos]
}

// newTail resumes a file at a persisted watermark. knownSize is the file
// size recorded when the watermark was saved: while the file still has
// exactly that size there is nothing new, so nothing is parsed; the
// first observed change triggers a replay from offset 0.
func newTail(path string, mark state.Watermark, knownSize int64) *tail {
	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return &tail{
		path:      path,
		sessionID: sessionID,
		parser:    transcript.NewParser(sessionID),
		readPos:   knownSize,
		lastSize:  knownSize,
		mark:      mark,
		synced:    knownSize == 0,
	}
}

func (t *tail) poll() (events []transcript.Event, grew bool, err error) {
	fi, err := os.Stat(t.path)
	if err != nil {
		return nil, false, err
	}
	size := fi.Size()
	grew = size != t.lastSize
	t.lastSize = size

	switch {
	case size < t.readPos:
		// Rewritten/truncated: prior offsets are invalid. Clear the
		// watermark — this file gets a fresh identity space.
		t.parser = transcript.NewParser(t.sessionID)
		t.readPos = 0
		t.mark = state.Watermark{}
		t.synced = true
	case !t.synced && size != t.readPos:
		// Resumed tail seeing its first change: replay from zero; the
		// watermark drops everything already emitted.
		t.parser = transcript.NewParser(t.sessionID)
		t.readPos = 0
		t.synced = true
	}
	if size == t.readPos {
		return nil, grew, nil
	}

	f, err := os.Open(t.path)
	if err != nil {
		return nil, grew, err
	}
	defer f.Close()
	if _, err := f.Seek(t.readPos, io.SeekStart); err != nil {
		return nil, grew, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err == nil {
			events = append(events, t.filter(t.parser.Line(t.readPos, line))...)
			t.readPos += int64(len(line))
			continue
		}
		if err == io.EOF {
			// A trailing line without '\n' may be mid-write: leave it
			// unconsumed; readPos does not advance past it.
			return events, grew, nil
		}
		return events, grew, err
	}
}

// filter drops events covered by the watermark and advances it.
func (t *tail) filter(evs []transcript.Event) []transcript.Event {
	var out []transcript.Event
	for _, ev := range evs {
		if t.mark.Covers(ev.Offset, ev.Seq) {
			continue
		}
		t.mark = state.Watermark{Offset: ev.Offset, Seq: ev.Seq, Set: true}
		if ev.Project != "" {
			t.project = ev.Project
		}
		out = append(out, ev)
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/watch/`
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/watch/
git commit -m "feat(watch): per-file tail — replay-from-zero resume with watermark dedupe"
```

---

### Task 5: Watcher core — scan, emit, persist

**Files:**
- Create: `internal/watch/watch.go`
- Test: `internal/watch/watch_test.go`

**Interfaces:**
- Consumes: Task 4's `tail` (`newTail`, `poll`, `.mark`, `.project`, `.sessionID`, `.lastSize`); `state.State`/`FileState`/`Watermark`; `redact.Apply`/`Level`; `transcript.Event`.
- Produces:
  - `type Sink interface { Emit(transcript.Event) error }`
  - `type Options struct { Roots []string; Machine string; Level redact.Level; IdleAfter, EndedAfter time.Duration; Backfill bool; Now func() time.Time }`
  - `func New(opts Options, st *state.State, sink Sink) *Watcher`
  - `(*Watcher).PollOnce() error` — one deterministic pass (the unit-test seam); returns the state-save error, per-file errors are counted in `Watcher.FileErrs int`.
  - `(*Watcher).Run(ctx context.Context, interval time.Duration) error` — PollOnce on a ticker until ctx is done.
- Timers are Task 6; this task wires scan → tail → machine stamp → redact → sink → state, plus first-sighting fast-forward and removed-file detection.

- [ ] **Step 1: Write the failing tests**

`internal/watch/watch_test.go`:

```go
package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/incantery/agentmon/internal/redact"
	"github.com/incantery/agentmon/internal/state"
	"github.com/incantery/agentmon/internal/transcript"
)

type collector struct{ events []transcript.Event }

func (c *collector) Emit(ev transcript.Event) error {
	c.events = append(c.events, ev)
	return nil
}

// newTestWatcher returns a watcher over a temp root plus helpers.
// clock is a *time.Time the test advances manually.
func newTestWatcher(t *testing.T, st *state.State, backfill bool) (*Watcher, *collector, string, *time.Time) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	c := &collector{}
	w := New(Options{
		Roots:      []string{root},
		Machine:    "m1",
		Level:      redact.Metadata,
		IdleAfter:  60 * time.Second,
		EndedAfter: 30 * time.Minute,
		Backfill:   backfill,
		Now:        func() time.Time { return now },
	}, st, c)
	return w, c, filepath.Join(root, "proj"), &now
}

func TestFirstSightingFastForwards(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, _ := newTestWatcher(t, st, false)
	path := filepath.Join(dir, "old.jsonl")
	write(t, path, line1+line2) // pre-existing history
	if err := w.PollOnce(); err != nil {
		t.Fatal(err)
	}
	if len(c.events) != 0 {
		t.Fatalf("fast-forward must not emit history, got %v", types(c.events))
	}
	// but new appends are emitted
	appendTo(t, path, `{"type":"permission-mode","permissionMode":"auto","sessionId":"old"}`+"\n")
	w.PollOnce()
	if got := types(c.events); len(got) != 1 || got[0] != transcript.PermissionMode {
		t.Fatalf("post-fast-forward append lost: %v", got)
	}
}

func TestBackfillEmitsHistory(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, _ := newTestWatcher(t, st, true)
	write(t, filepath.Join(dir, "old.jsonl"), line1)
	w.PollOnce()
	if got := types(c.events); len(got) != 2 {
		t.Fatalf("backfill should emit history: %v", got)
	}
}

func TestMachineStampAndRedaction(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, _ := newTestWatcher(t, st, true)
	write(t, filepath.Join(dir, "s1.jsonl"), line1)
	w.PollOnce()
	for _, ev := range c.events {
		if ev.Machine != "m1" {
			t.Errorf("machine not stamped: %+v", ev)
		}
	}
	// line1 is a user prompt "hello" — at metadata level the text must be gone
	last := c.events[len(c.events)-1]
	if p, ok := last.Payload.(transcript.UserPromptPayload); !ok || p.Text != "" || p.Chars != 5 {
		t.Errorf("redaction failed: %+v", last.Payload)
	}
}

func TestWatermarkPersistsAcrossWatchers(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	st, _ := state.Load(statePath)
	w, c, dir, _ := newTestWatcher(t, st, true)
	path := filepath.Join(dir, "s1.jsonl")
	write(t, path, line1)
	w.PollOnce()
	n := len(c.events)
	if n == 0 {
		t.Fatal("no events on first pass")
	}

	// "restart": fresh state from disk, fresh watcher over the same root
	st2, err := state.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	c2 := &collector{}
	w2 := New(Options{
		Roots: []string{filepath.Dir(dir)}, Machine: "m1", Level: redact.Metadata,
		IdleAfter: 60 * time.Second, EndedAfter: 30 * time.Minute,
		Now: func() time.Time { return time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC) },
	}, st2, c2)
	w2.PollOnce()
	if len(c2.events) != 0 {
		t.Fatalf("restart re-emitted %v", types(c2.events))
	}
	appendTo(t, path, line2)
	w2.PollOnce()
	if got := types(c2.events); len(got) != 1 || got[0] != transcript.SessionTitle {
		t.Fatalf("after restart+append: %v", got)
	}
}

func TestRemovedFileEmitsSessionEnded(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, _ := newTestWatcher(t, st, true)
	path := filepath.Join(dir, "s1.jsonl")
	write(t, path, line1)
	w.PollOnce()
	os.Remove(path)
	c.events = nil
	w.PollOnce()
	if len(c.events) != 1 || c.events[0].Type != transcript.SessionEnded {
		t.Fatalf("got %v", types(c.events))
	}
	if p := c.events[0].Payload.(transcript.SessionEndedPayload); p.Reason != "removed" {
		t.Errorf("reason = %q", p.Reason)
	}
	if _, ok := st.Files[path]; ok {
		t.Error("state entry not deleted")
	}
	c.events = nil
	w.PollOnce()
	if len(c.events) != 0 {
		t.Error("removed file reported twice")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/watch/`
Expected: new tests FAIL — `undefined: New`.

- [ ] **Step 3: Implement**

`internal/watch/watch.go`:

```go
package watch

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/incantery/agentmon/internal/redact"
	"github.com/incantery/agentmon/internal/state"
	"github.com/incantery/agentmon/internal/transcript"
)

// Sink receives fully-formed (machine-stamped, redacted) events.
type Sink interface {
	Emit(ev transcript.Event) error
}

type Options struct {
	Roots      []string
	Machine    string
	Level      redact.Level
	IdleAfter  time.Duration
	EndedAfter time.Duration
	Backfill   bool // emit pre-existing history on first sighting
	Now        func() time.Time
}

type Watcher struct {
	opts  Options
	st    *state.State
	sink  Sink
	tails map[string]*tail

	FileErrs int // per-file stat/read errors (never fatal)
	SinkErrs int
}

func New(opts Options, st *state.State, sink Sink) *Watcher {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Watcher{opts: opts, st: st, sink: sink, tails: map[string]*tail{}}
}

// PollOnce runs one deterministic pass over every transcript file:
// discover, tail, emit, update timers (Task 6), persist state.
func (w *Watcher) PollOnce() error {
	now := w.opts.Now()
	seen := map[string]bool{}
	for _, path := range w.scan() {
		seen[path] = true
		w.pollFile(path, now)
	}
	// Files we were tracking that vanished: the session is gone.
	for path, t := range w.tails {
		if seen[path] {
			continue
		}
		fs := w.st.File(path)
		if !fs.Ended {
			w.synthetic(t, fs, now, transcript.SessionEndedPayload{Reason: "removed"})
		}
		delete(w.tails, path)
		w.st.Delete(path)
	}
	return w.st.Save()
}

func (w *Watcher) pollFile(path string, now time.Time) {
	t, tracked := w.tails[path]
	_, known := w.st.Files[path] // must read BEFORE File() get-or-creates
	fs := w.st.File(path)
	if !tracked {
		if !known || !fs.Watermark.Set {
			// First sighting ever: fast-forward unless backfilling, so the
			// first run doesn't flood the sink with historical transcripts.
			fi, err := os.Stat(path)
			if err != nil {
				w.FileErrs++
				w.st.Delete(path)
				return
			}
			if !w.opts.Backfill {
				fs.Watermark = state.Watermark{Offset: fi.Size(), Seq: -1, Set: true}
				fs.Size = fi.Size()
			}
			w.initTimers(fs, fi, now)
		}
		t = newTail(path, fs.Watermark, fs.Size)
		w.tails[path] = t
	}

	evs, grewNow, err := t.poll()
	if err != nil {
		w.FileErrs++
		return
	}
	for _, ev := range evs {
		w.emit(ev)
	}
	fs.Watermark = t.mark
	fs.Size = t.lastSize
	if len(evs) > 0 {
		last := evs[len(evs)-1].Type
		fs.MidTurn = last != transcript.TurnCompleted && last != transcript.SessionEnded
	}
	w.tickTimers(t, fs, grewNow, now)
}

// emit stamps machine identity, applies the content level, and hands the
// event to the sink. Redaction here is the enforcement point the spec
// demands: nothing reaches a sink unredacted.
func (w *Watcher) emit(ev transcript.Event) {
	ev.Machine = w.opts.Machine
	if err := w.sink.Emit(redact.Apply(w.opts.Level, ev)); err != nil {
		w.SinkErrs++
	}
}

// synthetic emits a watcher-generated event, continuing the seq counter
// at the file's last offset so identity never collides with parser events.
func (w *Watcher) synthetic(t *tail, fs *state.FileState, now time.Time, pl transcript.Payload) {
	fs.Watermark.Seq++
	fs.Watermark.Set = true
	t.mark = fs.Watermark
	w.emit(transcript.Event{
		Project:   t.project,
		SessionID: t.sessionID,
		Offset:    fs.Watermark.Offset,
		Seq:       fs.Watermark.Seq,
		TS:        now,
		Type:      pl.EventType(),
		Payload:   pl,
	})
}

// initTimers and tickTimers are completed in the timers task; the core
// task only records activity so state is coherent.
func (w *Watcher) initTimers(fs *state.FileState, fi os.FileInfo, now time.Time) {
	fs.LastActivityUnix = fi.ModTime().Unix()
}

func (w *Watcher) tickTimers(t *tail, fs *state.FileState, grew bool, now time.Time) {
	if grew {
		fs.LastActivityUnix = now.Unix()
		fs.IdleFired = false
		fs.Ended = false
	}
}

func (w *Watcher) scan() []string {
	var out []string
	for _, root := range w.opts.Roots {
		matches, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
		if err != nil {
			continue
		}
		out = append(out, matches...)
	}
	sort.Strings(out)
	return out
}

// Run polls on a fixed interval until ctx is done.
func (w *Watcher) Run(ctx context.Context, interval time.Duration) error {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := w.PollOnce(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./...`
Expected: PASS (Task 4's tests plus 5 new).

- [ ] **Step 5: Commit**

```bash
git add internal/watch/
git commit -m "feat(watch): Watcher core — scan, fast-forward, stamp+redact+emit, persisted watermarks"
```

---

### Task 6: Timers — session_idle, session_ended, grandfathering

**Files:**
- Modify: `internal/watch/watch.go` (`initTimers`, `tickTimers`)
- Test: `internal/watch/watch_test.go` (append tests)

**Interfaces:**
- Consumes: Task 5's `synthetic`, `FileState` timer fields.
- Produces final timer behavior:
  - `session_idle{idle_seconds}` fires once when `MidTurn` and no activity for ≥ `IdleAfter`; re-arms on activity.
  - `session_ended{reason:"inactive"}` fires once after ≥ `EndedAfter` without activity; activity un-ends the session.
  - **Grandfathering:** at first sighting, `LastActivityUnix` = file mtime, and if the file is already older than `EndedAfter` it is marked `Ended` silently (no event) — a fleet of historical transcripts must not fire hundreds of session_ended events 30 minutes after the first run.

- [ ] **Step 1: Write the failing tests**

Append to `internal/watch/watch_test.go`:

```go
// midTurnLine leaves the session mid-turn (assistant message, no turn_completed)
const midTurnLine = `{"type":"assistant","message":{"model":"m","role":"assistant","content":[{"type":"text","text":"working"}],"usage":{"input_tokens":1,"output_tokens":1}},"timestamp":"2026-07-06T12:00:01.000Z","cwd":"/p","sessionId":"s1"}` + "\n"
const turnDoneLine = `{"type":"system","subtype":"turn_duration","durationMs":5,"messageCount":2,"timestamp":"2026-07-06T12:00:02.000Z","sessionId":"s1"}` + "\n"

func TestIdleFiresOnceMidTurnAndRearms(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, now := newTestWatcher(t, st, true)
	path := filepath.Join(dir, "s1.jsonl")
	write(t, path, line1+midTurnLine)
	w.PollOnce() // emits + records activity, MidTurn=true
	c.events = nil

	*now = now.Add(61 * time.Second)
	w.PollOnce()
	if got := types(c.events); len(got) != 1 || got[0] != transcript.SessionIdle {
		t.Fatalf("want one session_idle, got %v", got)
	}
	if p := c.events[0].Payload.(transcript.SessionIdlePayload); p.IdleSeconds < 60 {
		t.Errorf("idle_seconds = %d", p.IdleSeconds)
	}
	c.events = nil
	*now = now.Add(10 * time.Second)
	w.PollOnce()
	if len(c.events) != 0 {
		t.Fatalf("idle fired twice: %v", types(c.events))
	}
	// activity re-arms
	appendTo(t, path, midTurnLine)
	w.PollOnce()
	c.events = nil
	*now = now.Add(61 * time.Second)
	w.PollOnce()
	if got := types(c.events); len(got) != 1 || got[0] != transcript.SessionIdle {
		t.Fatalf("idle did not re-arm: %v", got)
	}
}

func TestNoIdleAfterTurnCompleted(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, now := newTestWatcher(t, st, true)
	write(t, filepath.Join(dir, "s1.jsonl"), line1+midTurnLine+turnDoneLine)
	w.PollOnce()
	c.events = nil
	*now = now.Add(2 * time.Minute)
	w.PollOnce()
	if len(c.events) != 0 {
		t.Fatalf("idle fired after turn_completed: %v", types(c.events))
	}
}

func TestEndedFiresOnceThenActivityResumes(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, now := newTestWatcher(t, st, true)
	path := filepath.Join(dir, "s1.jsonl")
	write(t, path, line1+turnDoneLine)
	w.PollOnce()
	c.events = nil
	*now = now.Add(31 * time.Minute)
	w.PollOnce()
	if got := types(c.events); len(got) != 1 || got[0] != transcript.SessionEnded {
		t.Fatalf("want one session_ended, got %v", got)
	}
	if p := c.events[0].Payload.(transcript.SessionEndedPayload); p.Reason != "inactive" {
		t.Errorf("reason = %q", p.Reason)
	}
	c.events = nil
	*now = now.Add(1 * time.Minute)
	w.PollOnce()
	if len(c.events) != 0 {
		t.Fatalf("ended fired twice: %v", types(c.events))
	}
	appendTo(t, path, midTurnLine)
	w.PollOnce()
	if got := types(c.events); len(got) != 1 || got[0] != transcript.AssistantMessage {
		t.Fatalf("resume after ended: %v", got)
	}
}

func TestHistoricalFilesGrandfatheredSilently(t *testing.T) {
	st, _ := state.Load("")
	w, c, dir, now := newTestWatcher(t, st, false)
	path := filepath.Join(dir, "ancient.jsonl")
	write(t, path, line1)
	oldTime := now.Add(-48 * time.Hour)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	w.PollOnce()
	*now = now.Add(31 * time.Minute)
	w.PollOnce()
	if len(c.events) != 0 {
		t.Fatalf("grandfathered file emitted %v", types(c.events))
	}
	if !st.File(path).Ended {
		t.Error("ancient file not marked Ended")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/watch/`
Expected: the 4 new tests FAIL (timers currently never fire).

- [ ] **Step 3: Implement**

Replace `initTimers` and `tickTimers` in `internal/watch/watch.go`:

```go
// initTimers seeds timer state at first sighting. Activity starts at the
// file's mtime, and a file already past EndedAfter is marked Ended
// SILENTLY — historical transcripts must not fire a wave of
// session_ended events when the watcher first runs on a machine.
func (w *Watcher) initTimers(fs *state.FileState, fi os.FileInfo, now time.Time) {
	fs.LastActivityUnix = fi.ModTime().Unix()
	if now.Sub(fi.ModTime()) >= w.opts.EndedAfter {
		fs.Ended = true
	}
}

func (w *Watcher) tickTimers(t *tail, fs *state.FileState, grew bool, now time.Time) {
	if grew {
		fs.LastActivityUnix = now.Unix()
		fs.IdleFired = false
		fs.Ended = false
		return
	}
	if fs.LastActivityUnix == 0 {
		fs.LastActivityUnix = now.Unix()
		return
	}
	idle := now.Sub(time.Unix(fs.LastActivityUnix, 0))
	if !fs.Ended && idle >= w.opts.EndedAfter {
		fs.Ended = true
		w.synthetic(t, fs, now, transcript.SessionEndedPayload{Reason: "inactive"})
		return
	}
	if fs.MidTurn && !fs.IdleFired && !fs.Ended && idle >= w.opts.IdleAfter {
		fs.IdleFired = true
		w.synthetic(t, fs, now, transcript.SessionIdlePayload{IdleSeconds: int64(idle.Seconds())})
	}
}
```

Note the grandfathering interaction: `initTimers` runs inside the first-sighting branch of `pollFile` (Task 5 already calls it), so `fs.Ended` is set before `tickTimers` ever runs for that file, and the "fires once" guard (`!fs.Ended`) suppresses the event.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./...`
Expected: PASS — including Task 5's tests (whose timer expectations don't change: their clocks never advance past the thresholds mid-test except where asserted).

- [ ] **Step 5: Commit**

```bash
git add internal/watch/
git commit -m "feat(watch): idle/ended timers with mid-turn gating, re-arm, and silent grandfathering"
```

---

### Task 7: `agentmon watch` CLI — flags, sinks, dry-run

**Files:**
- Create: `cmd/agentmon/watch.go`
- Modify: `cmd/agentmon/main.go` (register subcommand)
- Modify: `README.md` (watch section), `docs/superpowers/specs/2026-07-06-agentmon-design.md` (record poll-only + flags-not-TOML decisions)
- Test: `cmd/agentmon/watch_test.go`

**Interfaces:**
- Consumes: `watch.New/Options/Sink/PollOnce/Run`; `state.Load`; `spool.Open/Append`; `redact.Valid/Level`; `transcript.Event/SpoolEvictedPayload`.
- Produces: `agentmon watch` with flags:
  `--roots` (comma-separated, default `~/.claude/projects`), `--machine` (default `os.Hostname()`), `--level` (default `metadata`), `--interval` (default `2s`), `--idle-after` (default `60s`), `--ended-after` (default `30m`), `--state-file` (default `~/.local/state/agentmon/state.json`), `--spool-dir` (default `~/.local/state/agentmon/spool`), `--spool-max-mb` (default `256`), `--backfill`, `--dry-run` (stdout sink + in-memory state, touches nothing on disk), `--once` (single poll, then exit).
  Internal seam: `runWatch(stdout, stderr io.Writer, f watchFlags) error`.

- [ ] **Step 1: Write the failing test**

`cmd/agentmon/watch_test.go`:

```go
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWatchDryRunOnce(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","message":{"role":"user","content":"hi"},"timestamp":"2026-07-06T10:00:00.000Z","cwd":"/p","sessionId":"w1"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "w1.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	f := defaultWatchFlags()
	f.roots = root
	f.machine = "testbox"
	f.dryRun = true
	f.once = true
	f.backfill = true
	if err := runWatch(&out, &errb, f); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 { // session_started + user_prompt
		t.Fatalf("got %d lines:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"machine":"testbox"`) || !strings.Contains(lines[0], `"type":"session_started"`) {
		t.Errorf("first line: %s", lines[0])
	}
	if strings.Contains(out.String(), `"hi"`) {
		t.Error("metadata level leaked prompt text")
	}
}

func TestWatchSpoolsWhenNotDryRun(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "proj")
	os.MkdirAll(dir, 0o755)
	line := `{"type":"ai-title","aiTitle":"T","sessionId":"w1"}` + "\n"
	os.WriteFile(filepath.Join(dir, "w1.jsonl"), []byte(line), 0o644)

	work := t.TempDir()
	var out, errb bytes.Buffer
	f := defaultWatchFlags()
	f.roots = root
	f.machine = "testbox"
	f.once = true
	f.backfill = true
	f.stateFile = filepath.Join(work, "state.json")
	f.spoolDir = filepath.Join(work, "spool")
	if err := runWatch(&out, &errb, f); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("spool mode wrote to stdout: %s", out.String())
	}
	entries, err := os.ReadDir(f.spoolDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no spool segments: %v %v", entries, err)
	}
	if _, err := os.Stat(f.stateFile); err != nil {
		t.Errorf("state not persisted: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(f.spoolDir, entries[0].Name()))
	if !strings.Contains(string(data), `"type":"session_started"`) {
		t.Errorf("spool content: %s", data)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/agentmon/`
Expected: FAIL — `undefined: defaultWatchFlags`.

- [ ] **Step 3: Implement**

`cmd/agentmon/watch.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/incantery/agentmon/internal/redact"
	"github.com/incantery/agentmon/internal/spool"
	"github.com/incantery/agentmon/internal/state"
	"github.com/incantery/agentmon/internal/transcript"
	"github.com/incantery/agentmon/internal/watch"
)

type watchFlags struct {
	roots      string
	machine    string
	level      string
	interval   time.Duration
	idleAfter  time.Duration
	endedAfter time.Duration
	stateFile  string
	spoolDir   string
	spoolMaxMB int64
	backfill   bool
	dryRun     bool
	once       bool
}

func defaultWatchFlags() watchFlags {
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	return watchFlags{
		roots:      filepath.Join(home, ".claude", "projects"),
		machine:    host,
		level:      string(redact.Metadata),
		interval:   2 * time.Second,
		idleAfter:  60 * time.Second,
		endedAfter: 30 * time.Minute,
		stateFile:  filepath.Join(home, ".local", "state", "agentmon", "state.json"),
		spoolDir:   filepath.Join(home, ".local", "state", "agentmon", "spool"),
		spoolMaxMB: 256,
	}
}

// jsonSink prints events as JSON lines (dry-run mode).
type jsonSink struct{ enc *json.Encoder }

func (s jsonSink) Emit(ev transcript.Event) error { return s.enc.Encode(ev) }

// spoolSink appends events to the disk spool. When appending forces an
// eviction, a spool_evicted marker event is appended so the data loss is
// visible downstream (the marker's own eviction result is ignored to
// avoid recursion).
type spoolSink struct {
	sp      *spool.Spool
	machine string
}

func (s *spoolSink) Emit(ev transcript.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	evicted, err := s.sp.Append(b)
	if err != nil {
		return err
	}
	if evicted > 0 {
		now := time.Now().UTC()
		marker := transcript.Event{
			Machine:   s.machine,
			SessionID: "spool",
			Offset:    now.UnixNano(),
			TS:        now,
			Type:      transcript.SpoolEvicted,
			Payload:   transcript.SpoolEvictedPayload{Dropped: evicted},
		}
		if mb, err := json.Marshal(marker); err == nil {
			s.sp.Append(mb)
		}
	}
	return nil
}

func runWatch(stdout, stderr io.Writer, f watchFlags) error {
	if !redact.Valid(redact.Level(f.level)) {
		return fmt.Errorf("invalid --level %q", f.level)
	}
	var sink watch.Sink
	statePath := f.stateFile
	if f.dryRun {
		sink = jsonSink{enc: json.NewEncoder(stdout)}
		statePath = "" // in-memory: dry-run touches nothing on disk
	} else {
		sp, err := spool.Open(f.spoolDir, 4<<20, f.spoolMaxMB<<20)
		if err != nil {
			return err
		}
		defer sp.Close()
		sink = &spoolSink{sp: sp, machine: f.machine}
	}
	st, err := state.Load(statePath)
	if err != nil {
		return err
	}
	w := watch.New(watch.Options{
		Roots:      strings.Split(f.roots, ","),
		Machine:    f.machine,
		Level:      redact.Level(f.level),
		IdleAfter:  f.idleAfter,
		EndedAfter: f.endedAfter,
		Backfill:   f.backfill,
	}, st, sink)
	if f.once {
		err := w.PollOnce()
		if w.FileErrs > 0 || w.SinkErrs > 0 {
			fmt.Fprintf(stderr, "errors: file=%d sink=%d\n", w.FileErrs, w.SinkErrs)
		}
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := w.Run(ctx, f.interval); err != nil && err != context.Canceled {
		return err
	}
	return nil
}
```

In `cmd/agentmon/main.go`, extend the switch:

```go
	case "watch":
		f := defaultWatchFlags()
		fs := flag.NewFlagSet("watch", flag.ExitOnError)
		fs.StringVar(&f.roots, "roots", f.roots, "comma-separated transcript roots")
		fs.StringVar(&f.machine, "machine", f.machine, "machine name stamped on events")
		fs.StringVar(&f.level, "level", f.level, `content level: "metadata" or "full"`)
		fs.DurationVar(&f.interval, "interval", f.interval, "poll interval")
		fs.DurationVar(&f.idleAfter, "idle-after", f.idleAfter, "mid-turn inactivity before session_idle")
		fs.DurationVar(&f.endedAfter, "ended-after", f.endedAfter, "inactivity before session_ended")
		fs.StringVar(&f.stateFile, "state-file", f.stateFile, "watch state path")
		fs.StringVar(&f.spoolDir, "spool-dir", f.spoolDir, "spool directory")
		fs.Int64Var(&f.spoolMaxMB, "spool-max-mb", f.spoolMaxMB, "spool size cap (MB)")
		fs.BoolVar(&f.backfill, "backfill", false, "emit pre-existing history on first sighting")
		fs.BoolVar(&f.dryRun, "dry-run", false, "print events to stdout; no state or spool writes")
		fs.BoolVar(&f.once, "once", false, "poll once and exit")
		fs.Parse(os.Args[2:])
		if fs.NArg() != 0 {
			usage()
		}
		if err := runWatch(os.Stdout, os.Stderr, f); err != nil {
			fmt.Fprintln(os.Stderr, "agentmon:", err)
			os.Exit(1)
		}
```

and update `usage()` to mention both subcommands:

```go
	fmt.Fprintln(os.Stderr, `usage:
  agentmon parse [--level metadata|full] <session.jsonl>
  agentmon watch [--dry-run] [--once] [--backfill] [flags]`)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go build ./... && go test ./...`
Expected: PASS across all packages.

- [ ] **Step 5: Dogfood against the real machine**

```bash
make build
bin/agentmon watch --dry-run --once            # fast-forward: prints nothing (first sighting)
bin/agentmon watch --dry-run --once --backfill 2>/dev/null | head -5   # prints history
timeout 10 bin/agentmon watch --dry-run || true   # live: type in a Claude session, watch events stream
```

Expected: no panics; `--backfill` streams metadata-level events; the live run prints events as sessions write (or nothing if all sessions are quiet — both fine).

- [ ] **Step 6: Update docs**

README: under "Status", replace the milestone list line about what's coming with a `watch` section showing `bin/agentmon watch --dry-run` and the flag summary, and note "coming next: the ingest server (serve), alerts, service units."

Spec (`docs/superpowers/specs/2026-07-06-agentmon-design.md`): in the "Watching" section, replace the per-directory-watches sentence with: "Watching is poll-based (default 2s): stat known files plus a glob rescan per tick — no fsnotify dependency; latency is bounded by the interval, which is fine for a monitor. First sighting of a file fast-forwards (no history emitted) unless `--backfill` is set." In the "Config" section, add: "Milestone 2 ships flags only (stdlib has no TOML); the config file lands with `serve`."

- [ ] **Step 7: Commit**

```bash
git add cmd/agentmon/ README.md docs/
git commit -m "feat(cli): agentmon watch — poll-based tailing with dry-run, once, and spool sinks"
```

---

## Done when

- `go test ./...` green; `make build` produces `bin/agentmon`.
- `bin/agentmon watch --dry-run` on a machine with live Claude Code sessions streams metadata-level events within one poll interval of activity, survives restarts without duplicate identities, and never crashes on transcript weirdness.
- A non-dry-run watch populates `~/.local/state/agentmon/spool/` and `state.json`; restarting it re-emits nothing.
- Milestone 3 (serve + store) can consume the spool segments and the event identity scheme unchanged.
