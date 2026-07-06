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

func TestEvictErrorDoesNotFailAppend(t *testing.T) {
	dir := t.TempDir()
	sp, _ := Open(dir, 12, 20) // tiny: rotation + eviction pressure fast
	defer sp.Close()
	sp.Append([]byte("0123456789ab")) // fills segment 1 (13 bytes)
	sp.Append([]byte("12345"))        // rotates to segment 2, writes 6 bytes
	// Make the directory unwritable so os.Remove (eviction) fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	// Third append won't rotate (curSize=6 < segMax=12) but will trigger eviction.
	if _, err := sp.Append([]byte("0123456789ab")); err != nil {
		t.Fatalf("append must succeed even when eviction fails: %v", err)
	}
	if sp.EvictErrs == 0 {
		t.Error("eviction failure not counted in EvictErrs")
	}
}

func TestOpenIsLazyNoEmptySegments(t *testing.T) {
	dir := t.TempDir()
	sp, err := Open(dir, 1<<20, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	sp.Close()
	segs, _ := sp.Segments()
	if len(segs) != 0 {
		t.Fatalf("Open must not create segments: %v", segs)
	}
}

func TestRotateClosesCurrentForDraining(t *testing.T) {
	sp, _ := Open(t.TempDir(), 1<<20, 1<<30)
	defer sp.Close()
	sp.Append([]byte(`{"a":1}`))
	closed, _ := sp.ClosedSegments()
	if len(closed) != 0 {
		t.Fatalf("current segment must not be closed yet: %v", closed)
	}
	if err := sp.Rotate(); err != nil {
		t.Fatal(err)
	}
	closed, _ = sp.ClosedSegments()
	if len(closed) != 1 {
		t.Fatalf("after Rotate the segment must be closed: %v", closed)
	}
	// Rotate with nothing open is a no-op
	if err := sp.Rotate(); err != nil {
		t.Fatal(err)
	}
	closed2, _ := sp.ClosedSegments()
	if len(closed2) != 1 {
		t.Fatalf("empty Rotate created a segment: %v", closed2)
	}
	// next Append opens a fresh segment
	sp.Append([]byte(`{"b":2}`))
	segs, _ := sp.Segments()
	if len(segs) != 2 {
		t.Fatalf("append after Rotate should open segment 2: %v", segs)
	}
}

func TestAckDeletesClosedRefusesCurrent(t *testing.T) {
	sp, _ := Open(t.TempDir(), 1<<20, 1<<30)
	defer sp.Close()
	sp.Append([]byte(`{"a":1}`))
	sp.Rotate()
	sp.Append([]byte(`{"b":2}`)) // new current
	closed, _ := sp.ClosedSegments()
	if len(closed) != 1 {
		t.Fatalf("setup: %v", closed)
	}
	if err := sp.Ack(closed[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(closed[0]); !os.IsNotExist(err) {
		t.Error("Ack did not delete the segment")
	}
	segs, _ := sp.Segments()
	if len(segs) != 1 {
		t.Fatalf("segments after ack: %v", segs)
	}
	if err := sp.Ack(segs[0]); err == nil {
		t.Error("Ack must refuse the current segment")
	}
}

func TestConcurrentAppendAndDrainOps(t *testing.T) {
	sp, _ := Open(t.TempDir(), 64, 1<<30)
	defer sp.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			sp.Append([]byte(`{"n":1}`))
		}
	}()
	for i := 0; i < 50; i++ {
		if closed, err := sp.ClosedSegments(); err == nil {
			for _, c := range closed {
				sp.Ack(c)
			}
		}
		sp.Rotate()
	}
	<-done
}
