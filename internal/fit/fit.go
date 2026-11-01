// Package fit decides whether a registry model fits in the live free
// memory of this box (A4 of docs/work-board/PLAN.md). The registry
// records what each model needs (vram_gb for a full GPU placement,
// ram_gb for a CPU placement, both 0 for cloud models); a live reading
// of free VRAM (nvidia-smi) and free system RAM (MemAvailable in
// /proc/meminfo) is compared against it, and the verdict is one of
// fits, needs_offload, too_big. The bridge posts the verdict as a Plane
// comment when a planning job is queued.
package fit

import (
	"context"
	"fmt"
	"html"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// Outcome is the fit verdict for one model against a memory snapshot.
type Outcome string

const (
	// Fits: the model runs entirely in free GPU memory.
	Fits Outcome = "fits"
	// NeedsOffload: too big for the free VRAM, but a CPU placement
	// (ram_gb) fits in the free system RAM.
	NeedsOffload Outcome = "needs_offload"
	// TooBig: neither the VRAM nor the RAM can hold the model.
	TooBig Outcome = "too_big"
)

// Readings is a snapshot of live free memory, in GiB.
type Readings struct {
	FreeVRAMGB float64
	FreeRAMGB  float64
}

// Source reads a live free-memory snapshot.
type Source interface {
	Readings(ctx context.Context) (Readings, error)
}

// Decision returns the fit verdict for model against the snapshot:
// fits when the free VRAM covers vram_gb, otherwise needs_offload when
// the free RAM covers ram_gb, otherwise too_big. The comparison is
// "free >= needed", so a model that needs exactly what is free fits.
// Cloud models (0/0) always fit — they run on the provider's hardware,
// not this box's.
func Decision(m registry.Model, r Readings) Outcome {
	if r.FreeVRAMGB >= m.VRAMGB {
		return Fits
	}
	if r.FreeRAMGB >= m.RAMGB {
		return NeedsOffload
	}
	return TooBig
}

// Live is the Source that reads the real box: free GPU memory from
// nvidia-smi and free system RAM from MemAvailable in /proc/meminfo.
type Live struct {
	// Bin is the nvidia-smi executable (default "nvidia-smi").
	Bin string
	// MemInfo is the meminfo path (default "/proc/meminfo").
	MemInfo string
}

// Readings implements Source.
func (l Live) Readings(ctx context.Context) (Readings, error) {
	bin := l.Bin
	if bin == "" {
		bin = "nvidia-smi"
	}
	out, err := exec.CommandContext(ctx, bin,
		"--query-gpu=memory.free", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return Readings{}, fmt.Errorf("fit: nvidia-smi: %w", err)
	}
	vram, err := parseGPUFree(string(out))
	if err != nil {
		return Readings{}, err
	}
	ram, err := l.freeRAM()
	if err != nil {
		return Readings{}, err
	}
	return Readings{FreeVRAMGB: vram, FreeRAMGB: ram}, nil
}

// freeRAM reads MemAvailable from the (overridable) meminfo path.
func (l Live) freeRAM() (float64, error) {
	path := l.MemInfo
	if path == "" {
		path = "/proc/meminfo"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("fit: reading meminfo: %w", err)
	}
	return parseMemAvailable(data)
}

// parseGPUFree parses nvidia-smi memory.free output (MiB, one line per
// GPU) and returns the emptiest GPU's free VRAM in GiB — a model is
// placed on a single GPU, so that is the box's usable free VRAM.
func parseGPUFree(output string) (float64, error) {
	var bestMiB float64
	var sawLine bool
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		mib, err := strconv.ParseFloat(line, 64)
		if err != nil {
			return 0, fmt.Errorf("fit: parsing nvidia-smi free memory %q: %w", line, err)
		}
		if !sawLine || mib > bestMiB {
			bestMiB = mib
			sawLine = true
		}
	}
	if !sawLine {
		return 0, fmt.Errorf("fit: nvidia-smi reported no GPUs")
	}
	return bestMiB / 1024, nil
}

// parseMemAvailable parses /proc/meminfo and returns MemAvailable in GiB
// (the kernel's estimate of RAM actually usable, after caches and slab).
func parseMemAvailable(meminfo []byte) (float64, error) {
	for _, line := range strings.Split(string(meminfo), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return 0, fmt.Errorf("fit: parsing MemAvailable %q: %w", fields[1], err)
		}
		return kb / 1024 / 1024, nil
	}
	return 0, fmt.Errorf("fit: MemAvailable not found in meminfo")
}

// Comment is the Plane comment (HTML) for a fit verdict: the model, the
// verdict, the words behind it, and the numbers.
func Comment(m registry.Model, r Readings, o Outcome) string {
	return fmt.Sprintf(
		"<p>Fit check for %s (%s): <b>%s</b> — %s<br>"+
			"free VRAM %.1f GiB (needs %.1f GiB) · free RAM %.1f GiB (needs %.1f GiB)</p>",
		html.EscapeString(m.DisplayName), html.EscapeString(m.ID), o, reason(o),
		r.FreeVRAMGB, m.VRAMGB, r.FreeRAMGB, m.RAMGB)
}

// reason is the human half of the verdict.
func reason(o Outcome) string {
	switch o {
	case Fits:
		return "runs fully in GPU memory"
	case NeedsOffload:
		return "too big for free VRAM, run with CPU offload"
	default:
		return "does not fit in VRAM or RAM on this box"
	}
}
