package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/incantery/agentmon/internal/config"
	"github.com/incantery/agentmon/internal/drain"
	"github.com/incantery/agentmon/internal/loki"
	"github.com/incantery/agentmon/internal/redact"
	"github.com/incantery/agentmon/internal/spool"
	"github.com/incantery/agentmon/internal/state"
	"github.com/incantery/agentmon/internal/transcript"
	"github.com/incantery/agentmon/internal/watch"
)

type watchFlags struct {
	roots         string
	machine       string
	level         string
	interval      time.Duration
	idleAfter     time.Duration
	endedAfter    time.Duration
	stateFile     string
	spoolDir      string
	spoolMaxMB    int64
	backfill      bool
	dryRun        bool
	once          bool
	lokiURL       string
	lokiTenant    string
	lokiLabels    map[string]string
	drainInterval time.Duration
}

func watchFlagsFrom(cfg config.Config) watchFlags {
	return watchFlags{
		roots:         strings.Join(cfg.Watch.Roots, ","),
		machine:       cfg.Machine,
		level:         cfg.Watch.Level,
		interval:      cfg.Watch.Interval.D(),
		idleAfter:     cfg.Watch.IdleAfter.D(),
		endedAfter:    cfg.Watch.EndedAfter.D(),
		stateFile:     cfg.Watch.StateFile,
		spoolDir:      cfg.Watch.SpoolDir,
		spoolMaxMB:    cfg.Watch.SpoolMaxMB,
		lokiURL:       cfg.Loki.URL,
		lokiTenant:    cfg.Loki.Tenant,
		lokiLabels:    cfg.Loki.Labels,
		drainInterval: cfg.Loki.DrainInterval.D(),
	}
}

// configPathFromArgs pre-scans for --config before the FlagSet exists,
// because the config supplies the other flags' defaults.
func configPathFromArgs(args []string) string {
	for i, a := range args {
		if a == "--config" || a == "-config" {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1]
			}
		}
		for _, p := range []string{"--config=", "-config="} {
			if strings.HasPrefix(a, p) {
				return strings.TrimPrefix(a, p)
			}
		}
	}
	return config.DefaultPath()
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
	if ev.TS.IsZero() {
		// Stamp once at write time: the spooled line must carry a stable
		// timestamp so retried pushes stay byte-identical (Loki dedupe).
		ev.TS = time.Now().UTC()
	}
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
	var sp *spool.Spool
	statePath := f.stateFile
	if f.dryRun {
		sink = jsonSink{enc: json.NewEncoder(stdout)}
		statePath = "" // in-memory: dry-run touches nothing on disk
	} else {
		release, err := spool.AcquireLock(f.spoolDir)
		if err != nil {
			return err
		}
		defer release()
		sp, err = spool.Open(f.spoolDir, spool.DefaultSegmentBytes, f.spoolMaxMB<<20)
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
	var dr *drain.Drainer
	if !f.dryRun && f.lokiURL != "" {
		dr = drain.New(sp, loki.New(f.lokiURL, f.lokiTenant), drain.Options{StaticLabels: f.lokiLabels})
	}
	if f.once {
		err := w.PollOnce()
		if w.FileErrs > 0 || w.SinkErrs > 0 {
			fmt.Fprintf(stderr, "errors: file=%d sink=%d\n", w.FileErrs, w.SinkErrs)
		}
		if dr != nil {
			if err := sp.Rotate(); err != nil {
				fmt.Fprintln(stderr, "agentmon: rotate:", err)
			}
			shipped, drErr := dr.DrainOnce()
			if drErr != nil {
				fmt.Fprintln(stderr, "agentmon: drain:", drErr)
			}
			fmt.Fprintf(stderr, "drain: shipped %d segment(s), quarantined %d\n", shipped, dr.Quarantined)
		}
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var drainDone chan struct{}
	if dr != nil {
		drainDone = make(chan struct{})
		go func() {
			defer close(drainDone)
			tick := time.NewTicker(f.drainInterval)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if err := sp.Rotate(); err != nil {
						fmt.Fprintln(stderr, "agentmon: rotate:", err)
					}
					if _, err := dr.DrainOnce(); err != nil {
						fmt.Fprintln(stderr, "agentmon: drain:", err)
					}
				}
			}
		}()
	}
	runErr := w.Run(ctx, f.interval)
	stop() // cancel ctx so the drain goroutine exits even on hard errors
	if drainDone != nil {
		<-drainDone
	}
	if runErr != nil && runErr != context.Canceled {
		return runErr
	}
	return nil
}
