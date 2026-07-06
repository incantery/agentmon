// Package state persists per-file watch progress: the ship watermark
// (identity of the last emitted event) plus the timer bookkeeping the
// watcher needs across restarts.
package state

import (
	"bytes"
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
	SynthSeq         int       `json:"synth_seq,omitempty"` // count of watcher-synthesized events emitted for this file
	Project          string    `json:"project,omitempty"`   // last known project, for synthetics emitted before any parser event this run
}

type State struct {
	path      string
	Files     map[string]*FileState `json:"files"`
	lastSaved []byte
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
	if bytes.Equal(data, s.lastSaved) {
		return nil
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
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	s.lastSaved = data
	return nil
}
