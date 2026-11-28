package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
		{"negative loopd.shutdown_grace_seconds", func(c *Config) { c.Loopd.ShutdownGraceSeconds = -1 }},
		{"empty loopd.listen", func(c *Config) { c.Loopd.Listen = "" }},
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

func TestLoopdDefaults(t *testing.T) {
	c := Default()
	if got, want := c.Loopd.TokenFile, "/run/loopd/token"; got != want {
		t.Errorf("Loopd.TokenFile = %q, want %q", got, want)
	}
	if got, want := c.Loopd.ShutdownGraceSeconds, 60; got != want {
		t.Errorf("Loopd.ShutdownGraceSeconds = %d, want %d", got, want)
	}
	if got, want := c.Loopd.Listen, ":9110"; got != want {
		t.Errorf("Loopd.Listen = %q, want %q", got, want)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Default() with loopd keys does not validate: %v", err)
	}
}

func TestLoopdNegativeGraceRejected(t *testing.T) {
	c := Default()
	c.Loopd.ShutdownGraceSeconds = -1
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for negative loopd.shutdown_grace_seconds")
	}
}

func TestLoopdUnknownKeyRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loopd.yaml")
	if err := os.WriteFile(path, []byte("loopd:\n  bogus_key: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load(file with unknown loopd key) = nil error, want an error")
	}
}

func TestPermissionsLoadFromYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loopd.yaml")
	yaml := "permissions:\n" +
		"  allow:\n" +
		"    - tool: read_file\n" +
		"      pattern: \"docs/*.md\"\n" +
		"  deny:\n" +
		"    - tool: shell\n" +
		"      pattern: \"rm*\"\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if want := []PermRule{{Tool: "read_file", Pattern: "docs/*.md"}}; !reflect.DeepEqual(c.Permissions.Allow, want) {
		t.Errorf("Permissions.Allow = %v, want %v", c.Permissions.Allow, want)
	}
	if want := []PermRule{{Tool: "shell", Pattern: "rm*"}}; !reflect.DeepEqual(c.Permissions.Deny, want) {
		t.Errorf("Permissions.Deny = %v, want %v", c.Permissions.Deny, want)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for a loaded permission config", err)
	}
}

func TestPermissionsDefaultEmpty(t *testing.T) {
	c := Default()
	if len(c.Permissions.Allow) != 0 {
		t.Errorf("Default().Permissions.Allow = %v, want empty", c.Permissions.Allow)
	}
	if len(c.Permissions.Deny) != 0 {
		t.Errorf("Default().Permissions.Deny = %v, want empty", c.Permissions.Deny)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Default().Validate() = %v, want nil (empty permissions are valid)", err)
	}
}

func TestPermissionsEmptyToolRejected(t *testing.T) {
	c := Default()
	c.Permissions.Allow = []PermRule{{Tool: "", Pattern: "*.md"}}
	if err := c.Validate(); err == nil {
		t.Error("Validate() = nil, want an error for an empty allow tool")
	}
	// The deny list is checked the same way.
	c2 := Default()
	c2.Permissions.Deny = []PermRule{{Tool: "", Pattern: "rm*"}}
	if err := c2.Validate(); err == nil {
		t.Error("Validate() = nil, want an error for an empty deny tool")
	}
}

func TestPermissionsBadPatternRejected(t *testing.T) {
	c := Default()
	c.Permissions.Allow = []PermRule{{Tool: "read_file", Pattern: "[unclosed"}}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for a malformed permission pattern")
	}
	// A valid pattern on the same list still passes, so the rejection is
	// about the pattern, not the tool.
	c.Permissions.Allow = []PermRule{{Tool: "read_file", Pattern: "*.md"}}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for a well-formed pattern", err)
	}
}

func TestWorkspaceDefaults(t *testing.T) {
	c := Default()
	if got, want := c.Workspace.Root, "/var/lib/jarvis/workspaces"; got != want {
		t.Errorf("Workspace.Root = %q, want %q", got, want)
	}
	if got, want := c.Workspace.Repo, "/home/agent/ai-agent-build"; got != want {
		t.Errorf("Workspace.Repo = %q, want %q", got, want)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Default() with workspace keys does not validate: %v", err)
	}
}

func TestWorkspaceEmptyRepoRejected(t *testing.T) {
	c := Default()
	c.Workspace.Repo = ""
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "workspace.repo") {
		t.Fatalf("Validate() with empty workspace.repo = %v, want an error naming workspace.repo", err)
	}
	// The root is checked the same way.
	c2 := Default()
	c2.Workspace.Root = ""
	if err := c2.Validate(); err == nil {
		t.Error("Validate() = nil, want an error for an empty workspace.root")
	}
}
