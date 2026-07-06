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
