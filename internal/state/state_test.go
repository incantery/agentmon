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

func TestSaveSkipsWhenUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Load(path)
	s.File("/x").Size = 1
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	// Clobber the file directly; an unchanged re-Save must NOT rewrite it.
	if err := os.WriteFile(path, []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "sentinel" {
		t.Error("Save rewrote the file despite unchanged state")
	}
	// A real change must write again.
	s.File("/x").Size = 2
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if string(data) == "sentinel" {
		t.Error("Save skipped a genuine change")
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
