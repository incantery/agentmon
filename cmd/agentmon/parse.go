package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/incantery/agentmon/internal/redact"
	"github.com/incantery/agentmon/internal/transcript"
)

// runParse streams one transcript file through the parser and prints each
// derived event as a JSON line. The session ID is the filename stem, as in
// ~/.claude/projects/<project>/<session-uuid>.jsonl.
func runParse(stdout, stderr io.Writer, path string, level redact.Level) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	p := transcript.NewParser(sessionID)
	enc := json.NewEncoder(stdout)

	// ReadBytes (not Scanner): transcript lines routinely exceed Scanner's
	// default token limit.
	r := bufio.NewReaderSize(f, 1<<20)
	var offset int64
	for {
		line, err := r.ReadBytes('\n')
		for _, ev := range p.Line(offset, line) {
			if encErr := enc.Encode(redact.Apply(level, ev)); encErr != nil {
				return encErr
			}
		}
		offset += int64(len(line))
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if len(p.Skipped) > 0 {
		fmt.Fprintf(stderr, "skipped: %v\n", p.Skipped)
	}
	return nil
}
