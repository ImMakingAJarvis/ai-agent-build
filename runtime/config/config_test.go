package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultValid(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("Default() does not validate: %v", err)
	}
	if got, want := c.Engine.URL, "http://127.0.0.1:11434"; got != want {
		t.Errorf("Engine.URL = %q, want %q", got, want)
	}
	if got, want := c.Engine.SlotDir, "/var/lib/jarvis/slots"; got != want {
		t.Errorf("Engine.SlotDir = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(c.Slots, []int{0}) {
		t.Errorf("Slots = %v, want [0]", c.Slots)
	}
	if got, want := c.SnapshotMinTokens, 900; got != want { // docs/runtime/measurements.md
		t.Errorf("SnapshotMinTokens = %d, want %d", got, want)
	}
	if got, want := c.InlineWaitMS, 300; got != want {
		t.Errorf("InlineWaitMS = %d, want %d", got, want)
	}
	if got, want := c.SpillBytes, 8192; got != want {
		t.Errorf("SpillBytes = %d, want %d", got, want)
	}
	if got, want := c.MaxTicks, 200; got != want {
		t.Errorf("MaxTicks = %d, want %d", got, want)
	}
	if got, want := c.MaxTaskTokens, 100000; got != want {
		t.Errorf("MaxTaskTokens = %d, want %d", got, want)
	}
	if got, want := c.AfterEditTimeoutSeconds, 300; got != want {
		t.Errorf("AfterEditTimeoutSeconds = %d, want %d", got, want)
	}
	got := [4]int{c.Timeouts.QuickSeconds, c.Timeouts.NormalSeconds, c.Timeouts.MaxOpHours, c.Timeouts.StallMinutes}
	if want := [4]int{30, 600, 24, 10}; got != want {
		t.Errorf("Timeouts = %v, want %v", got, want)
	}
}

func TestLoadMissingFileIsDefault(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Load(missing file) = %v, want no error", err)
	}
	if !reflect.DeepEqual(c, Default()) {
		t.Errorf("Load(missing file) =\n%+v\nwant Default()\n%+v", c, Default())
	}
}

func TestLoadOverlay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loopd.yaml")
	if err := os.WriteFile(path, []byte("slots: [0, 1]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	want := Default()
	want.Slots = []int{0, 1}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("Load(overlay) =\n%+v\nwant (only Slots changed)\n%+v", c, want)
	}
}

func TestLoadUnknownKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loopd.yaml")
	if err := os.WriteFile(path, []byte("slots: [0]\nbogus_key: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load(file with unknown key) = nil error, want an error")
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"no slots", func(c *Config) { c.Slots = nil }},
		{"empty slots", func(c *Config) { c.Slots = []int{} }},
		{"negative slot", func(c *Config) { c.Slots = []int{-1} }},
		{"empty engine url", func(c *Config) { c.Engine.URL = "" }},
		{"negative snapshot_min_tokens", func(c *Config) { c.SnapshotMinTokens = -1 }},
		{"negative inline_wait_ms", func(c *Config) { c.InlineWaitMS = -1 }},
		{"negative spill_bytes", func(c *Config) { c.SpillBytes = -1 }},
		{"negative max_ticks", func(c *Config) { c.MaxTicks = -1 }},
		{"negative max_task_tokens", func(c *Config) { c.MaxTaskTokens = -1 }},
		{"negative after_edit_timeout_seconds", func(c *Config) { c.AfterEditTimeoutSeconds = -1 }},
		{"negative quick_seconds", func(c *Config) { c.Timeouts.QuickSeconds = -1 }},
		{"negative normal_seconds", func(c *Config) { c.Timeouts.NormalSeconds = -1 }},
		{"negative max_op_hours", func(c *Config) { c.Timeouts.MaxOpHours = -1 }},
		{"negative stall_minutes", func(c *Config) { c.Timeouts.StallMinutes = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("Validate() = nil, want an error for %s", tc.name)
			}
		})
	}
}
