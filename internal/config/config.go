// Package config loads agentmon's per-machine TOML config. Precedence is
// defaults < config file < explicitly passed flags (the flag layer lives
// in cmd/agentmon: flag defaults are seeded FROM the loaded config, so a
// flag overrides only when the user actually passes it).
package config

import (
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration is a time.Duration that unmarshals from TOML strings ("90s").
type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

type Config struct {
	Machine string `toml:"machine"`
	Watch   Watch  `toml:"watch"`
	Loki    Loki   `toml:"loki"`
}

type Watch struct {
	Level      string   `toml:"level"`
	Roots      []string `toml:"roots"`
	Interval   Duration `toml:"interval"`
	IdleAfter  Duration `toml:"idle_after"`
	EndedAfter Duration `toml:"ended_after"`
	StateFile  string   `toml:"state_file"`
	SpoolDir   string   `toml:"spool_dir"`
	SpoolMaxMB int64    `toml:"spool_max_mb"`
}

type Loki struct {
	URL           string            `toml:"url"`
	Tenant        string            `toml:"tenant"`
	Labels        map[string]string `toml:"labels"`
	DrainInterval Duration          `toml:"drain_interval"`
}

func Default() Config {
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	return Config{
		Machine: host,
		Watch: Watch{
			Level:      "metadata",
			Roots:      []string{filepath.Join(home, ".claude", "projects")},
			Interval:   Duration(2 * time.Second),
			IdleAfter:  Duration(60 * time.Second),
			EndedAfter: Duration(30 * time.Minute),
			StateFile:  filepath.Join(home, ".local", "state", "agentmon", "state.json"),
			SpoolDir:   filepath.Join(home, ".local", "state", "agentmon", "spool"),
			SpoolMaxMB: 256,
		},
		Loki: Loki{
			DrainInterval: Duration(10 * time.Second),
		},
	}
}

// Load reads path over the defaults. A missing file is not an error.
func Load(path string) (Config, error) {
	c := Default()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return c, nil
	}
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return c, err
	}
	return c, nil
}

func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "agentmon", "config.toml")
}
