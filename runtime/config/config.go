// Package config is the runtime's one YAML config file (M2.4, card
// ELR-23): every number in the plan is a key here, never a constant in
// code. Load overlays a file on Default(); the file lives at $LOOPD_CONFIG
// (default ~/.config/jarvis/loopd.yaml) and is resolved by the caller.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

// Config is the runtime configuration. Every field is a key in the YAML
// file; anything not in the file keeps its default.
type Config struct {
	Engine struct {
		URL     string `yaml:"url"`
		SlotDir string `yaml:"slot_dir"`
	} `yaml:"engine"`
	Slots                   []int    `yaml:"slots"`
	SnapshotMinTokens       int      `yaml:"snapshot_min_tokens"`
	InlineWaitMS            int      `yaml:"inline_wait_ms"`
	SpillBytes              int      `yaml:"spill_bytes"`
	MaxTicks                int      `yaml:"max_ticks"`
	MaxTaskTokens           int      `yaml:"max_task_tokens"`
	AfterEdit               []string `yaml:"after_edit"`
	AfterEditTimeoutSeconds int      `yaml:"after_edit_timeout_seconds"`
	Timeouts                struct {
		QuickSeconds  int `yaml:"quick_seconds"`
		NormalSeconds int `yaml:"normal_seconds"`
		MaxOpHours    int `yaml:"max_op_hours"`
		StallMinutes  int `yaml:"stall_minutes"`
	} `yaml:"timeouts"`
	InstructionFiles []string `yaml:"instruction_files"`
}

// Default returns the runtime defaults. snapshot_min_tokens is 900, the
// value decided in docs/runtime/measurements.md ("Decision:
// snapshot_min_tokens = 900") — measured, not guessed.
func Default() Config {
	var c Config
	c.Engine.URL = "http://127.0.0.1:11434"
	c.Engine.SlotDir = "/var/lib/jarvis/slots"
	c.Slots = []int{0}
	c.SnapshotMinTokens = 900
	c.InlineWaitMS = 300
	c.SpillBytes = 8192
	c.MaxTicks = 200
	c.MaxTaskTokens = 100000
	c.AfterEditTimeoutSeconds = 300
	c.Timeouts.QuickSeconds = 30
	c.Timeouts.NormalSeconds = 600
	c.Timeouts.MaxOpHours = 24
	c.Timeouts.StallMinutes = 10
	return c
}

// Load reads the YAML file at path and overlays it on Default(): keys the
// file does not set keep their defaults. A missing file (or an empty one)
// returns Default() and no error; unknown keys are an error, so a typo'd
// key fails loudly instead of being silently ignored.
func Load(path string) (Config, error) {
	c := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return c, nil // empty file: all defaults
		}
		return Default(), fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

// Validate reports a config that cannot run: slots must not be empty, no
// number may be negative, and engine.url must be set.
func (c Config) Validate() error {
	if len(c.Slots) == 0 {
		return fmt.Errorf("slots: must not be empty")
	}
	for i, s := range c.Slots {
		if s < 0 {
			return fmt.Errorf("slots[%d]: %d is negative", i, s)
		}
	}
	if c.Engine.URL == "" {
		return fmt.Errorf("engine.url: empty")
	}
	if c.SnapshotMinTokens < 0 {
		return fmt.Errorf("snapshot_min_tokens: %d is negative", c.SnapshotMinTokens)
	}
	if c.InlineWaitMS < 0 {
		return fmt.Errorf("inline_wait_ms: %d is negative", c.InlineWaitMS)
	}
	if c.SpillBytes < 0 {
		return fmt.Errorf("spill_bytes: %d is negative", c.SpillBytes)
	}
	if c.MaxTicks < 0 {
		return fmt.Errorf("max_ticks: %d is negative", c.MaxTicks)
	}
	if c.MaxTaskTokens < 0 {
		return fmt.Errorf("max_task_tokens: %d is negative", c.MaxTaskTokens)
	}
	if c.AfterEditTimeoutSeconds < 0 {
		return fmt.Errorf("after_edit_timeout_seconds: %d is negative", c.AfterEditTimeoutSeconds)
	}
	if c.Timeouts.QuickSeconds < 0 {
		return fmt.Errorf("timeouts.quick_seconds: %d is negative", c.Timeouts.QuickSeconds)
	}
	if c.Timeouts.NormalSeconds < 0 {
		return fmt.Errorf("timeouts.normal_seconds: %d is negative", c.Timeouts.NormalSeconds)
	}
	if c.Timeouts.MaxOpHours < 0 {
		return fmt.Errorf("timeouts.max_op_hours: %d is negative", c.Timeouts.MaxOpHours)
	}
	if c.Timeouts.StallMinutes < 0 {
		return fmt.Errorf("timeouts.stall_minutes: %d is negative", c.Timeouts.StallMinutes)
	}
	return nil
}

// YAML renders the config as YAML. Marshalling a plain data struct cannot
// fail; a failure here is a bug in this package, so panic.
func (c Config) YAML() string {
	b, err := yaml.Marshal(c)
	if err != nil {
		panic(fmt.Sprintf("config: marshal: %v", err))
	}
	return string(b)
}
