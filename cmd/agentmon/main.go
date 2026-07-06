// agentmon: telemetry for AI coding-agent sessions.
// Milestone 1 ships only the parse debug command; watch/serve/sessions
// arrive in later milestones (see docs/superpowers/specs/).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/incantery/agentmon/internal/redact"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  agentmon parse [--level metadata|full] <session.jsonl>
  agentmon watch [--dry-run] [--once] [--backfill] [flags]`)
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
		f := defaultWatchFlags()
		fs := flag.NewFlagSet("watch", flag.ExitOnError)
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
		fs.Parse(os.Args[2:])
		if fs.NArg() != 0 {
			usage()
		}
		if err := runWatch(os.Stdout, os.Stderr, f); err != nil {
			fmt.Fprintln(os.Stderr, "agentmon:", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}
