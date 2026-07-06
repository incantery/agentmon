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
	fmt.Fprintln(os.Stderr, `usage: agentmon parse [--level metadata|full] <session.jsonl>`)
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
	default:
		usage()
	}
}
