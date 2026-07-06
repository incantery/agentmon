// agentmon: telemetry for AI coding-agent sessions.
// Milestone 1 ships only the parse debug command; watch/serve/sessions
// arrive in later milestones (see docs/superpowers/specs/).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/incantery/agentmon/internal/config"
	"github.com/incantery/agentmon/internal/redact"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  agentmon parse [--level metadata|full] <session.jsonl>
  agentmon watch [--config PATH] [--dry-run] [--once] [--backfill] [flags]
  agentmon drain [--config PATH] [--once]`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "parse":
		fs := flag.NewFlagSet("parse", flag.ExitOnError)
		level := fs.String("level", string(redact.Full), `content level: "metadata" or "full"`)
		fs.Parse(os.Args[2:])
		if fs.NArg() != 1 || !redact.Valid(redact.Level(*level)) {
			usage()
		}
		if err := runParse(os.Stdout, os.Stderr, fs.Arg(0), redact.Level(*level)); err != nil {
			fmt.Fprintln(os.Stderr, "agentmon:", err)
			os.Exit(1)
		}
	case "watch":
		cfg, err := config.Load(configPathFromArgs(os.Args[2:]))
		if err != nil {
			fmt.Fprintln(os.Stderr, "agentmon:", err)
			os.Exit(1)
		}
		f := watchFlagsFrom(cfg)
		fs := flag.NewFlagSet("watch", flag.ExitOnError)
		fs.String("config", config.DefaultPath(), "config file (already applied; here for -h)")
		fs.StringVar(&f.roots, "roots", f.roots, "comma-separated transcript roots")
		fs.StringVar(&f.machine, "machine", f.machine, "machine name stamped on events")
		fs.StringVar(&f.level, "level", f.level, `content level: "metadata" or "full"`)
		fs.DurationVar(&f.interval, "interval", f.interval, "poll interval")
		fs.DurationVar(&f.idleAfter, "idle-after", f.idleAfter, "mid-turn inactivity before session_idle")
		fs.DurationVar(&f.endedAfter, "ended-after", f.endedAfter, "inactivity before session_ended")
		fs.StringVar(&f.stateFile, "state-file", f.stateFile, "watch state path")
		fs.StringVar(&f.spoolDir, "spool-dir", f.spoolDir, "spool directory")
		fs.Int64Var(&f.spoolMaxMB, "spool-max-mb", f.spoolMaxMB, "spool size cap (MB)")
		fs.BoolVar(&f.backfill, "backfill", false, "emit pre-existing history on first sighting")
		fs.BoolVar(&f.dryRun, "dry-run", false, "print events to stdout; no state or spool writes")
		fs.BoolVar(&f.once, "once", false, "poll once and exit")
		fs.StringVar(&f.lokiURL, "loki-url", f.lokiURL, "Loki base URL (enables the drain loop)")
		fs.DurationVar(&f.drainInterval, "drain-interval", f.drainInterval, "spool→Loki drain interval")
		fs.Parse(os.Args[2:])
		if fs.NArg() != 0 {
			usage()
		}
		if err := runWatch(os.Stdout, os.Stderr, f); err != nil {
			fmt.Fprintln(os.Stderr, "agentmon:", err)
			os.Exit(1)
		}
	case "drain":
		cfg, err := config.Load(configPathFromArgs(os.Args[2:]))
		if err != nil {
			fmt.Fprintln(os.Stderr, "agentmon:", err)
			os.Exit(1)
		}
		fs := flag.NewFlagSet("drain", flag.ExitOnError)
		fs.String("config", config.DefaultPath(), "config file (already applied; here for -h)")
		once := fs.Bool("once", false, "drain once and exit")
		lokiURL := fs.String("loki-url", cfg.Loki.URL, "Loki base URL")
		fs.Parse(os.Args[2:])
		if fs.NArg() != 0 {
			usage()
		}
		cfg.Loki.URL = *lokiURL
		if err := runDrain(os.Stdout, os.Stderr, cfg, *once); err != nil {
			fmt.Fprintln(os.Stderr, "agentmon:", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}
