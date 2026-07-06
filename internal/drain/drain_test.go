package drain

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/incantery/agentmon/internal/loki"
	"github.com/incantery/agentmon/internal/spool"
)

const (
	evA = `{"machine":"m1","session_id":"s1","offset":0,"seq":0,"ts":"2026-07-08T10:00:00Z","type":"session_started","payload":{}}`
	evB = `{"machine":"m1","session_id":"s1","offset":10,"seq":0,"ts":"2026-07-08T10:00:05Z","type":"user_prompt","payload":{"chars":3}}`
	evC = `{"machine":"m1","session_id":"s1","offset":20,"seq":0,"type":"session_title","payload":{"title":"T"}}` // no ts
)

type capture struct {
	bodies [][]byte
	status int
}

func (c *capture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.bodies = append(c.bodies, b)
		w.WriteHeader(c.status)
	}
}

func newSpoolWith(t *testing.T, lines ...string) *spool.Spool {
	t.Helper()
	sp, err := spool.Open(t.TempDir(), 1<<20, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if _, err := sp.Append([]byte(l)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sp.Rotate(); err != nil {
		t.Fatal(err)
	}
	return sp
}

func TestDrainShipsAndAcks(t *testing.T) {
	cap := &capture{status: http.StatusNoContent}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	sp := newSpoolWith(t, evA, evB, evC)
	defer sp.Close()
	d := New(sp, loki.New(srv.URL, ""), Options{
		StaticLabels: map[string]string{"env": "lab"},
		Now:          func() time.Time { return time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC) },
	})
	shipped, err := d.DrainOnce()
	if err != nil || shipped != 1 {
		t.Fatalf("shipped=%d err=%v", shipped, err)
	}
	closed, _ := sp.ClosedSegments()
	if len(closed) != 0 {
		t.Fatalf("segment not acked: %v", closed)
	}
	if len(cap.bodies) != 1 {
		t.Fatalf("want 1 push, got %d", len(cap.bodies))
	}
	var body struct {
		Streams []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(cap.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Streams) != 3 { // three distinct types → three streams
		t.Fatalf("streams: %d", len(body.Streams))
	}
	for _, s := range body.Streams {
		if s.Stream["job"] != "agentmon" || s.Stream["machine"] != "m1" || s.Stream["env"] != "lab" {
			t.Errorf("labels: %v", s.Stream)
		}
		if _, bad := s.Stream["session_id"]; bad {
			t.Error("session_id must never be a label")
		}
		if s.Stream["type"] == "user_prompt" && s.Values[0][1] != evB {
			t.Errorf("line not byte-identical: %s", s.Values[0][1])
		}
		if s.Stream["type"] == "session_title" && s.Values[0][0] != "1783504805000000000" {
			t.Errorf("missing-ts fallback should reuse previous line's ts, got %s", s.Values[0][0])
		}
	}
}

func TestReservedLabelsWinOverStaticExtras(t *testing.T) {
	cap := &capture{status: http.StatusNoContent}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	sp := newSpoolWith(t, evA)
	defer sp.Close()
	d := New(sp, loki.New(srv.URL, ""), Options{
		StaticLabels: map[string]string{"type": "evil", "env": "lab"},
	})
	shipped, err := d.DrainOnce()
	if err != nil || shipped != 1 {
		t.Fatalf("shipped=%d err=%v", shipped, err)
	}
	var body struct {
		Streams []struct {
			Stream map[string]string `json:"stream"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(cap.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Streams) != 1 {
		t.Fatalf("streams: %d", len(body.Streams))
	}
	s := body.Streams[0].Stream
	if s["type"] != "session_started" {
		t.Errorf("reserved label 'type' clobbered by static extra: %v", s)
	}
	if s["env"] != "lab" {
		t.Errorf("non-reserved static extra lost: %v", s)
	}
}

func TestRetryableErrorStopsAndKeepsSegments(t *testing.T) {
	cap := &capture{status: http.StatusInternalServerError}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	sp := newSpoolWith(t, evA)
	defer sp.Close()
	d := New(sp, loki.New(srv.URL, ""), Options{})
	shipped, err := d.DrainOnce()
	if err == nil || shipped != 0 {
		t.Fatalf("want retryable error, got shipped=%d err=%v", shipped, err)
	}
	closed, _ := sp.ClosedSegments()
	if len(closed) != 1 {
		t.Fatalf("segment must survive a retryable failure: %v", closed)
	}
}

func TestPermanentRejectionQuarantines(t *testing.T) {
	cap := &capture{status: http.StatusBadRequest}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	sp := newSpoolWith(t, evA)
	defer sp.Close()
	d := New(sp, loki.New(srv.URL, ""), Options{})
	shipped, err := d.DrainOnce()
	if err != nil || shipped != 0 {
		t.Fatalf("permanent rejection must not fail the pass: shipped=%d err=%v", shipped, err)
	}
	if d.Quarantined != 1 {
		t.Errorf("Quarantined = %d", d.Quarantined)
	}
	closed, _ := sp.ClosedSegments()
	if len(closed) != 0 {
		t.Fatalf("rejected segment still listed: %v", closed)
	}
	// the bytes are preserved as .rej, visible for manual recovery
	dir := filepath.Dir(segDirProbe(t, sp))
	entries, _ := os.ReadDir(dir)
	var rej int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".rej") {
			rej++
		}
	}
	if rej != 1 {
		t.Errorf(".rej files = %d", rej)
	}
}

// segDirProbe recovers the spool dir from a fresh segment.
func segDirProbe(t *testing.T, sp *spool.Spool) string {
	t.Helper()
	if _, err := sp.Append([]byte(`{"probe":1}`)); err != nil {
		t.Fatal(err)
	}
	segs, _ := sp.Segments()
	return segs[len(segs)-1]
}

func TestMalformedTailSkipped(t *testing.T) {
	cap := &capture{status: http.StatusNoContent}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	sp := newSpoolWith(t, evA, `{torn`)
	defer sp.Close()
	d := New(sp, loki.New(srv.URL, ""), Options{})
	if shipped, err := d.DrainOnce(); err != nil || shipped != 1 {
		t.Fatalf("shipped=%d err=%v", shipped, err)
	}
}

func TestFullyCorruptSegmentQuarantinedNotAcked(t *testing.T) {
	cap := &capture{status: http.StatusNoContent}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	sp := newSpoolWith(t, `{torn`, `also not json`)
	defer sp.Close()
	d := New(sp, loki.New(srv.URL, ""), Options{})
	shipped, err := d.DrainOnce()
	if err != nil || shipped != 0 {
		t.Fatalf("shipped=%d err=%v", shipped, err)
	}
	if len(cap.bodies) != 0 {
		t.Error("no push should happen for a zero-stream segment")
	}
	if d.Quarantined != 1 {
		t.Errorf("Quarantined = %d, want 1 (corrupt bytes must leave a trail)", d.Quarantined)
	}
	closed, _ := sp.ClosedSegments()
	if len(closed) != 0 {
		t.Fatalf("corrupt segment still queued: %v", closed)
	}
}

func TestEmptySegmentAckedSilently(t *testing.T) {
	dir := t.TempDir()
	sp, _ := spool.Open(dir, 1<<20, 1<<30)
	defer sp.Close()
	// a genuinely empty segment (e.g. crash between create and write)
	if err := os.WriteFile(filepath.Join(dir, "spool-00000001.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cap := &capture{status: http.StatusNoContent}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	d := New(sp, loki.New(srv.URL, ""), Options{})
	shipped, err := d.DrainOnce()
	if err != nil || shipped != 1 {
		t.Fatalf("shipped=%d err=%v", shipped, err)
	}
	if d.Quarantined != 0 || len(cap.bodies) != 0 {
		t.Errorf("empty segment must ack silently: q=%d pushes=%d", d.Quarantined, len(cap.bodies))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("dir not empty: %v", entries)
	}
}
