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
