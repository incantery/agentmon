package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/incantery/agentmon/internal/config"
	"github.com/incantery/agentmon/internal/spool"
)

func TestDrainOnceStandalone(t *testing.T) {
	var pushed int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushed++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	spoolDir := t.TempDir()
	sp, _ := spool.Open(spoolDir, 1<<20, 1<<30)
	sp.Append([]byte(`{"machine":"m1","type":"session_title","ts":"2026-07-08T10:00:00Z"}`))
	sp.Rotate()
	sp.Close()

	cfg := config.Default()
	cfg.Watch.SpoolDir = spoolDir
	cfg.Loki.URL = srv.URL
	var out, errb bytes.Buffer
	if err := runDrain(&out, &errb, cfg, true); err != nil {
		t.Fatal(err)
	}
	if pushed != 1 {
		t.Errorf("pushes = %d", pushed)
	}
	entries, _ := os.ReadDir(spoolDir)
	for _, e := range entries {
		// AcquireLock's dir/.lock is expected to remain — only segments
		// (spool-*.jsonl) indicate an undrained spool.
		if strings.HasSuffix(e.Name(), ".jsonl") {
			t.Errorf("spool not drained: %v", entries)
		}
	}
}

func TestDrainWithoutLokiURLErrors(t *testing.T) {
	cfg := config.Default()
	cfg.Loki.URL = ""
	var out, errb bytes.Buffer
	if err := runDrain(&out, &errb, cfg, true); err == nil {
		t.Error("drain without [loki].url must error")
	}
}

func TestDrainRefusesWhenSpoolLocked(t *testing.T) {
	spoolDir := t.TempDir()
	release, err := spool.AcquireLock(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cfg := config.Default()
	cfg.Watch.SpoolDir = spoolDir
	cfg.Loki.URL = "http://127.0.0.1:1"
	var out, errb bytes.Buffer
	if err := runDrain(&out, &errb, cfg, true); err == nil {
		t.Error("drain must refuse a locked spool")
	}
}
