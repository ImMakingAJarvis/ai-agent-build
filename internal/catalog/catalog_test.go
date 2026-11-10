package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validDoc is the shape of config/capabilities.yaml: a capability can
// list several placements, each with the full worker field set.
const validDoc = `
capabilities:
  echo:
    description: "test capability"
    workers:
      - location: cpu
        model: echo
        vram_mb: 0
        ram_mb: 64
        needs_brain_out: false
        start: ""
        stop: ""
        typical_runtime: 30s
        idle_s: 0
  image.generate:
    description: "text-to-image"
    workers:
      - location: gpu
        model: zimage-turbo
        vram_mb: 12288
        ram_mb: 4096
        needs_brain_out: true
        start: "docker compose up -d art"
        stop: "docker compose stop art"
        typical_runtime: 45s
        idle_s: 300
      - location: cpu
        model: sdxl-turbo-cpu
        vram_mb: 0
        ram_mb: 16384
        needs_brain_out: false
        start: ""
        stop: ""
        typical_runtime: 5m
        idle_s: 0
`

func TestParseValid(t *testing.T) {
	f, err := Parse([]byte(validDoc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := f.SortedNames(); len(got) != 2 || got[0] != "echo" || got[1] != "image.generate" {
		t.Errorf("SortedNames = %v, want [echo image.generate]", got)
	}
	echo := f.ByName("echo")
	if echo == nil {
		t.Fatal("ByName(echo) = nil")
	}
	if echo.Name != "echo" {
		t.Errorf("echo.Name = %q, want echo", echo.Name)
	}
	if echo.Workers[0].Model != "echo" || echo.Workers[0].RAMMB != 64 {
		t.Errorf("echo worker = %+v, want model echo ram 64", echo.Workers[0])
	}
	img := f.ByName("image.generate")
	if img == nil {
		t.Fatal("ByName(image.generate) = nil")
	}
	if len(img.Workers) != 2 {
		t.Fatalf("image.generate has %d workers, want 2", len(img.Workers))
	}
	if img.Workers[0].VRAMMB != 12288 || !img.Workers[0].NeedsBrainOut {
		t.Errorf("gpu placement = %+v, want 12288 MB + needs_brain_out", img.Workers[0])
	}
	if img.Workers[0].Stop != "docker compose stop art" {
		t.Errorf("gpu stop = %q", img.Workers[0].Stop)
	}
	if f.ByName("nope") != nil {
		t.Error("ByName(nope) should be nil")
	}
}

func TestParseMissingField(t *testing.T) {
	doc := strings.Replace(validDoc, "        vram_mb: 0\n", "", 1) // echo's gpu-less worker
	f, err := Parse([]byte(doc))
	if f != nil {
		t.Fatalf("Parse succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "vram_mb") || !strings.Contains(err.Error(), "echo") {
		t.Errorf("error %q should name the capability and the missing field", err)
	}
}

func TestParseNoWorkers(t *testing.T) {
	doc := `
capabilities:
  echo:
    description: "test"
    workers: []
`
	if f, err := Parse([]byte(doc)); f != nil || err == nil {
		t.Errorf("Parse = %v, %v; want an error about workers", f, err)
	}
}

func TestParseUnknownLocation(t *testing.T) {
	doc := strings.Replace(validDoc, "location: cpu", "location: tpu", 1)
	if f, err := Parse([]byte(doc)); f != nil || err == nil {
		t.Errorf("Parse = %v, %v; want an unknown-location error", f, err)
	}
}

func TestParseRemoteLocation(t *testing.T) {
	doc := strings.Replace(validDoc, "location: cpu", "location: remote:worker-1", 1)
	f, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := f.ByName("echo").Workers[0].Cost(); got != "runs off-box" {
		t.Errorf("remote cost = %q, want runs off-box", got)
	}
}

func TestParseBadRuntime(t *testing.T) {
	doc := strings.Replace(validDoc, "typical_runtime: 45s", "typical_runtime: forty-five", 1)
	if f, err := Parse([]byte(doc)); f != nil || err == nil {
		t.Errorf("Parse = %v, %v; want a duration error", f, err)
	}
}

func TestParseEmpty(t *testing.T) {
	if f, err := Parse([]byte("capabilities: {}\n")); f != nil || err == nil {
		t.Errorf("Parse = %v, %v; want a no-capabilities error", f, err)
	}
}

func TestCostAndRuntime(t *testing.T) {
	f, err := Parse([]byte(validDoc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	echo := f.ByName("echo")
	if got := echo.Cost(); got != "runs alongside you on cpu" {
		t.Errorf("echo.Cost = %q", got)
	}
	if got, _ := echo.TypicalRuntime(); got != 30*time.Second {
		t.Errorf("echo.TypicalRuntime = %v, want 30s", got)
	}
	img := f.ByName("image.generate")
	if got := img.Cost(); got != "gpu:needs brain out | cpu:runs alongside you on cpu" {
		t.Errorf("image.generate.Cost = %q", got)
	}
	// The fastest placement sets the bar the model waits against.
	if got, _ := img.TypicalRuntime(); got != 45*time.Second {
		t.Errorf("image.generate.TypicalRuntime = %v, want 45s (the fast placement)", got)
	}
	if got := img.Locations(); len(got) != 2 || got[0] != "gpu" || got[1] != "cpu" {
		t.Errorf("image.generate.Locations = %v", got)
	}
	// A gpu placement without needs_brain_out co-schedules in free VRAM.
	if got := (Worker{Location: "gpu"}).Cost(); got != "runs alongside you in free VRAM" {
		t.Errorf("co-scheduled gpu cost = %q", got)
	}
}

func TestLoadFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capabilities.yaml")
	if err := os.WriteFile(path, []byte(validDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Capabilities) != 2 {
		t.Errorf("Load parsed %d capabilities, want 2", len(f.Capabilities))
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("Load of a missing file should fail")
	}
}
