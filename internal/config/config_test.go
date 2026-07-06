package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c := Default()
	if c.Watch.Level != "metadata" || c.Watch.Interval.D() != 2*time.Second ||
		c.Watch.IdleAfter.D() != 60*time.Second || c.Watch.EndedAfter.D() != 30*time.Minute ||
		c.Watch.SpoolMaxMB != 256 || c.Loki.DrainInterval.D() != 10*time.Second {
		t.Errorf("defaults wrong: %+v", c)
	}
	if c.Machine == "" || len(c.Watch.Roots) != 1 {
		t.Errorf("machine/roots defaults wrong: %+v", c)
	}
	if c.Loki.URL != "" {
		t.Error("loki must be off by default")
	}
}

func TestLoadMissingFileIsDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Watch.Level != "metadata" {
		t.Errorf("got %+v", c)
	}
}

func TestLoadOverlaysDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
machine = "workbox"

[watch]
level = "full"
idle_after = "90s"

[loki]
url = "http://lab:3100"
tenant = "seth"
drain_interval = "5s"
labels = { env = "lab" }
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Machine != "workbox" || c.Watch.Level != "full" || c.Watch.IdleAfter.D() != 90*time.Second {
		t.Errorf("overlay wrong: %+v", c)
	}
	// untouched keys keep defaults
	if c.Watch.EndedAfter.D() != 30*time.Minute || c.Watch.SpoolMaxMB != 256 {
		t.Errorf("defaults lost on overlay: %+v", c.Watch)
	}
	if c.Loki.URL != "http://lab:3100" || c.Loki.Tenant != "seth" ||
		c.Loki.DrainInterval.D() != 5*time.Second || c.Loki.Labels["env"] != "lab" {
		t.Errorf("loki wrong: %+v", c.Loki)
	}
}

func TestLoadBadTOMLErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte(`machine = [unclosed`), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("bad TOML must error")
	}
}

func TestBadDurationErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[watch]\nidle_after = \"soon\"\n"), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("bad duration must error")
	}
}
