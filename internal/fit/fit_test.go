package fit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

func model(vramGB, ramGB float64) registry.Model {
	return registry.Model{ID: "test-model", DisplayName: "Test Model", VRAMGB: vramGB, RAMGB: ramGB}
}

// TestDecision is the A4 acceptance criterion: mocked nvidia-smi/memory
// readings drive all three outcomes.
func TestDecision(t *testing.T) {
	local := model(20, 80)  // the MoE shape: 20 GiB VRAM, 80 GiB RAM
	dense := model(20, 0)   // the dense 27B shape: VRAM, CPU fallback is cheap
	cloud := model(0, 0)    // cloud models need nothing on this box
	tests := []struct {
		name string
		m    registry.Model
		r    Readings
		want Outcome
	}{
		{"fits: free VRAM covers the model", local, Readings{FreeVRAMGB: 24, FreeRAMGB: 50}, Fits},
		{"fits: exactly the required VRAM", local, Readings{FreeVRAMGB: 20, FreeRAMGB: 0}, Fits},
		{"fits: cloud model with no free memory at all", cloud, Readings{}, Fits},
		{"needs_offload: VRAM short, free RAM covers", local, Readings{FreeVRAMGB: 10, FreeRAMGB: 100}, NeedsOffload},
		{"needs_offload: exactly the required RAM", local, Readings{FreeVRAMGB: 0, FreeRAMGB: 80}, NeedsOffload},
		{"needs_offload: dense model offloads to CPU", dense, Readings{FreeVRAMGB: 5, FreeRAMGB: 0}, NeedsOffload},
		{"too_big: neither VRAM nor RAM covers", local, Readings{FreeVRAMGB: 10, FreeRAMGB: 50}, TooBig},
		{"too_big: nothing free", local, Readings{}, TooBig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Decision(tt.m, tt.r); got != tt.want {
				t.Errorf("Decision = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseGPUFree(t *testing.T) {
	got, err := parseGPUFree("2504\n")
	if err != nil {
		t.Fatalf("single GPU: %v", err)
	}
	if want := 2504.0 / 1024; got != want {
		t.Errorf("single GPU = %v GiB, want %v", got, want)
	}
	got, err = parseGPUFree(" 2504 \n16000\n")
	if err != nil {
		t.Fatalf("multi GPU: %v", err)
	}
	if want := 16000.0 / 1024; got != want {
		t.Errorf("multi GPU = %v GiB, want %v (the emptiest GPU)", got, want)
	}
	if _, err = parseGPUFree("garbage\n"); err == nil {
		t.Error("garbage input should fail")
	}
	if _, err = parseGPUFree(""); err == nil {
		t.Error("no GPUs should fail")
	}
}

func TestParseMemAvailable(t *testing.T) {
	got, err := parseMemAvailable([]byte("MemTotal:       128661436 kB\nMemFree:        50000000 kB\nMemAvailable:   59946448 kB\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := 59946448.0 / 1024 / 1024; got != want {
		t.Errorf("MemAvailable = %v GiB, want %v", got, want)
	}
	if _, err = parseMemAvailable([]byte("MemTotal: 1 kB\n")); err == nil {
		t.Error("missing MemAvailable should fail")
	}
}

// TestLiveReadings runs the real Live source against a fake nvidia-smi
// (a script) and a temp meminfo, so the exec and parse plumbing is
// exercised end to end.
func TestLiveReadings(t *testing.T) {
	dir := t.TempDir()
	meminfo := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(meminfo,
		[]byte("MemTotal:       128661436 kB\nMemAvailable:   10485760 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fake-smi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 20480\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := Live{Bin: bin, MemInfo: meminfo}
	r, err := l.Readings(context.Background())
	if err != nil {
		t.Fatalf("Readings: %v", err)
	}
	if r.FreeVRAMGB != 20 { // 20480 MiB
		t.Errorf("FreeVRAMGB = %v, want 20", r.FreeVRAMGB)
	}
	if r.FreeRAMGB != 10 { // 10485760 kB
		t.Errorf("FreeRAMGB = %v, want 10", r.FreeRAMGB)
	}
}

// TestLiveReadingsNoGPU: a missing nvidia-smi is an error, not a guess.
func TestLiveReadingsNoGPU(t *testing.T) {
	dir := t.TempDir()
	meminfo := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(meminfo, []byte("MemAvailable: 1048576 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := Live{Bin: filepath.Join(dir, "no-such-binary"), MemInfo: meminfo}
	if _, err := l.Readings(context.Background()); err == nil {
		t.Error("missing nvidia-smi should fail")
	}
}

func TestComment(t *testing.T) {
	m := model(20, 80)
	got := Comment(m, Readings{FreeVRAMGB: 10, FreeRAMGB: 100}, NeedsOffload)
	for _, want := range []string{"needs_offload", "Test Model", "test-model", "10.0 GiB", "80.0 GiB"} {
		if !strings.Contains(got, want) {
			t.Errorf("comment %q missing %q", got, want)
		}
	}
	// HTML-in-the-registry-name must not break out of the comment.
	evil := registry.Model{ID: "evil", DisplayName: "<script>alert(1)</script>", VRAMGB: 1, RAMGB: 1}
	if got := Comment(evil, Readings{}, TooBig); strings.Contains(got, "<script>") {
		t.Errorf("comment not escaped: %q", got)
	}
}
