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
	"sync"
	"syscall"
)

type Spool struct {
	mu        sync.Mutex
	dir       string
	segMax    int64
	totalMax  int64
	cur       *os.File
	curSize   int64
	curIndex  int
	EvictErrs int // eviction failures (old data not removed); the line writes themselves succeeded
}

// Open prepares the spool directory but does not create a segment file —
// the first Append opens one lazily. This keeps a drain-only process
// (which never appends) from littering empty segment files.
func Open(dir string, segMaxBytes, totalMaxBytes int64) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Spool{dir: dir, segMax: segMaxBytes, totalMax: totalMaxBytes}
	segs, err := s.segmentsLocked()
	if err != nil {
		return nil, err
	}
	for _, seg := range segs {
		var n int
		if _, err := fmt.Sscanf(filepath.Base(seg), "spool-%08d.jsonl", &n); err == nil && n > s.curIndex {
			s.curIndex = n
		}
	}
	return s, nil
}

func (s *Spool) rotateLocked() error {
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
// cap. The returned error refers to persisting THIS line; eviction
// problems are counted in EvictErrs instead (an eviction failure means
// old data was NOT removed — the new line itself is safely on disk).
// It returns how many previously spooled event lines were dropped to
// stay under the cap (0 in the normal case).
func (s *Spool) Append(line []byte) (evicted int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil || s.curSize >= s.segMax {
		if err := s.rotateLocked(); err != nil {
			return 0, err
		}
	}
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	n, err := s.cur.Write(buf)
	s.curSize += int64(n)
	if err != nil {
		// A torn write may have left a partial line at this segment's
		// tail. Poison the segment so the next Append rotates: the
		// corruption stays confined to this segment's final line, which
		// downstream JSONL readers treat as malformed-and-skipped.
		if s.curSize < s.segMax {
			s.curSize = s.segMax
		}
		return 0, err
	}
	dropped, evictErr := s.evictLocked()
	if evictErr != nil {
		s.EvictErrs++
	}
	return dropped, nil
}

func (s *Spool) evictLocked() (int, error) {
	dropped := 0
	for {
		segs, err := s.segmentsLocked()
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

// segmentsLocked returns all segment paths, oldest first (current segment
// last). Callers must already hold s.mu.
func (s *Spool) segmentsLocked() ([]string, error) {
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

// Segments returns all segment paths, oldest first (current segment last).
func (s *Spool) Segments() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.segmentsLocked()
}

// Rotate closes the current segment (if it has content) so the drainer
// can ship it without waiting for the size threshold. The next Append
// lazily opens a fresh segment. A no-op when nothing is open.
func (s *Spool) Rotate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil || s.curSize == 0 {
		return nil
	}
	err := s.cur.Close()
	s.cur = nil
	s.curSize = 0
	return err
}

// ClosedSegments returns every segment except the open current one,
// oldest first — the set the drainer may ship and Ack.
func (s *Spool) ClosedSegments() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	segs, err := s.segmentsLocked()
	if err != nil {
		return nil, err
	}
	if s.cur == nil {
		return segs, nil
	}
	curName := s.cur.Name()
	out := segs[:0]
	for _, p := range segs {
		if p != curName {
			out = append(out, p)
		}
	}
	return out, nil
}

// Ack deletes a shipped segment. It refuses the open current segment.
// A segment already removed (e.g. by cap eviction racing the drainer) counts as acked.
func (s *Spool) Ack(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && path == s.cur.Name() {
		return fmt.Errorf("spool: refusing to ack the current segment %s", path)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// AcquireLock takes an exclusive advisory lock on dir (via dir/.lock),
// guarding the spool against a second writer or a concurrent standalone
// drainer — the cross-process analogue of the in-process mutex. Returns
// a release func, or an error if another process holds the lock.
func AcquireLock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("spool at %s is locked by another agentmon process (watch already drains every tick): %w", dir, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return nil
	}
	err := s.cur.Close()
	s.cur = nil
	return err
}
