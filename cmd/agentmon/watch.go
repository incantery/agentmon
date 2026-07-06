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
			Seq:       -2, // synthetic identity space (negative seq)
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
