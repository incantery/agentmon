// Package drain ships closed spool segments to Loki and deletes each one
// only after Loki acknowledges it (2xx) — the at-least-once leg between
// the on-disk spool and the home-lab backend. Event lines pass through
// byte-identical with stable timestamps, so Loki's exact-duplicate
// dropping absorbs replays.
package drain

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"time"

	"github.com/incantery/agentmon/internal/loki"
	"github.com/incantery/agentmon/internal/spool"
)

type Options struct {
	StaticLabels map[string]string
	Now          func() time.Time
}

type Drainer struct {
	sp   *spool.Spool
	cl   *loki.Client
	opts Options

	// Quarantined counts segments Loki permanently rejected, renamed to
	// *.rej beside the spool — preserved for manual recovery, never
	// silently dropped, never blocking the queue.
	Quarantined int
	// QuarantineErrs counts failed quarantine renames (segment left in place, retried next pass).
	QuarantineErrs int
}

func New(sp *spool.Spool, cl *loki.Client, opts Options) *Drainer {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Drainer{sp: sp, cl: cl, opts: opts}
}

// DrainOnce ships every closed segment, oldest first. A retryable error
// stops the pass (remaining segments wait for the next tick); a
// permanent rejection quarantines that segment and continues.
func (d *Drainer) DrainOnce() (shipped int, err error) {
	segs, err := d.sp.ClosedSegments()
	if err != nil {
		return 0, err
	}
	for _, seg := range segs {
		streams, err := d.segmentStreams(seg)
		if err != nil {
			d.quarantine(seg)
			continue
		}
		if len(streams) == 0 {
			// Distinguish "nothing there" from "nothing decodable":
			// deleting undecodable bytes would be silent data loss.
			if fi, statErr := os.Stat(seg); statErr == nil && fi.Size() > 0 {
				d.quarantine(seg)
				continue
			}
			if err := d.sp.Ack(seg); err != nil {
				return shipped, err
			}
			shipped++
			continue
		}
		if err := d.cl.Push(streams); err != nil {
			var perm *loki.PermanentError
			if errors.As(err, &perm) {
				d.quarantine(seg)
				continue
			}
			return shipped, err
		}
		if err := d.sp.Ack(seg); err != nil {
			return shipped, err
		}
		shipped++
	}
	return shipped, nil
}

func (d *Drainer) quarantine(seg string) {
	if err := os.Rename(seg, seg+".rej"); err != nil {
		// The segment stays in place and will be retried next pass;
		// surface the failure instead of inflating Quarantined.
		d.QuarantineErrs++
		return
	}
	d.Quarantined++
}

// segmentStreams groups a segment's lines into Loki streams keyed by
// (machine, type). Lines are NOT re-marshaled — they ship byte-identical.
func (d *Drainer) segmentStreams(path string) ([]loki.Stream, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	type minimal struct {
		Machine string    `json:"machine"`
		Type    string    `json:"type"`
		TS      time.Time `json:"ts"`
	}
	byKey := map[string]*loki.Stream{}
	var lastTS time.Time
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var m minimal
		if err := json.Unmarshal(line, &m); err != nil {
			continue // torn tail from a poisoned segment: skip, not fatal
		}
		ts := m.TS
		if ts.IsZero() {
			ts = lastTS
		}
		if ts.IsZero() {
			ts = d.opts.Now()
		}
		lastTS = ts
		labels := map[string]string{"job": "agentmon", "machine": m.Machine, "type": m.Type}
		for k, v := range d.opts.StaticLabels {
			labels[k] = v
		}
		key := m.Machine + "\xff" + m.Type
		st, ok := byKey[key]
		if !ok {
			st = &loki.Stream{Labels: labels}
			byKey[key] = st
		}
		st.Entries = append(st.Entries, loki.Entry{TS: ts, Line: append([]byte(nil), line...)})
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]loki.Stream, 0, len(keys))
	for _, k := range keys {
		st := byKey[k]
		sort.SliceStable(st.Entries, func(i, j int) bool { return st.Entries[i].TS.Before(st.Entries[j].TS) })
		out = append(out, *st)
	}
	return out, nil
}
