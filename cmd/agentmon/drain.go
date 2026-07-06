package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/incantery/agentmon/internal/config"
	"github.com/incantery/agentmon/internal/drain"
	"github.com/incantery/agentmon/internal/loki"
	"github.com/incantery/agentmon/internal/spool"
)

// runDrain ships the configured spool to Loki — the standalone path for
// when watch isn't running (or a manual kick). Lazy spool open means this
// never creates segments of its own.
func runDrain(stdout, stderr io.Writer, cfg config.Config, once bool) error {
	if cfg.Loki.URL == "" {
		return fmt.Errorf("no [loki] url configured (%s)", config.DefaultPath())
	}
	release, err := spool.AcquireLock(cfg.Watch.SpoolDir)
	if err != nil {
		return err
	}
	defer release()
	sp, err := spool.Open(cfg.Watch.SpoolDir, spool.DefaultSegmentBytes, cfg.Watch.SpoolMaxMB<<20)
	if err != nil {
		return err
	}
	defer sp.Close()
	d := drain.New(sp, loki.New(cfg.Loki.URL, cfg.Loki.Tenant), drain.Options{StaticLabels: cfg.Loki.Labels})
	pass := func() error {
		shipped, err := d.DrainOnce()
		if shipped > 0 || d.Quarantined > 0 {
			fmt.Fprintf(stderr, "shipped %d segment(s), quarantined %d\n", shipped, d.Quarantined)
		}
		return err
	}
	if once {
		return pass()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tick := time.NewTicker(cfg.Loki.DrainInterval.D())
	defer tick.Stop()
	for {
		if err := pass(); err != nil {
			fmt.Fprintln(stderr, "agentmon: drain:", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
