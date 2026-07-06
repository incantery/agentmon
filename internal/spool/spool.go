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
