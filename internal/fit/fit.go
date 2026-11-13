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

// Readings is a snapshot of live memory, in GiB. Free* are what is
// available right now; Total* are the box's capacities (read live, so a
// machine swap is picked up without a code change).
type Readings struct {
	FreeVRAMGB  float64
	FreeRAMGB   float64
	TotalVRAMGB float64
	TotalRAMGB  float64
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
		"--query-gpu=memory.free,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return Readings{}, fmt.Errorf("fit: nvidia-smi: %w", err)
	}
	freeVRAM, totalVRAM, err := parseGPUMem(string(out))
	if err != nil {
		return Readings{}, err
	}
	freeRAM, totalRAM, err := l.ram()
	if err != nil {
		return Readings{}, err
	}
	return Readings{FreeVRAMGB: freeVRAM, FreeRAMGB: freeRAM, TotalVRAMGB: totalVRAM, TotalRAMGB: totalRAM}, nil
}

// ram reads MemAvailable and MemTotal from the (overridable) meminfo
// path.
func (l Live) ram() (free, total float64, err error) {
	path := l.MemInfo
	if path == "" {
		path = "/proc/meminfo"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("fit: reading meminfo: %w", err)
	}
	free, err = parseMemField(data, "MemAvailable:")
	if err != nil {
		return 0, 0, err
	}
	total, err = parseMemField(data, "MemTotal:")
	return free, total, err
}

// parseGPUMem parses nvidia-smi memory.free,memory.total output (MiB,
// "free, total" per line, one line per GPU) and returns the emptiest
// GPU's free and total VRAM in GiB — a model is placed on a single GPU,
// so that GPU is the box's usable VRAM.
func parseGPUMem(output string) (free, total float64, err error) {
	var bestFreeMiB float64
	var sawLine bool
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 2 {
			return 0, 0, fmt.Errorf("fit: parsing nvidia-smi memory %q: want \"free, total\"", line)
		}
		freeMiB, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		if err != nil {
			return 0, 0, fmt.Errorf("fit: parsing nvidia-smi free memory %q: %w", parts[0], err)
		}
		totalMiB, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil {
			return 0, 0, fmt.Errorf("fit: parsing nvidia-smi total memory %q: %w", parts[1], err)
		}
		if !sawLine || freeMiB > bestFreeMiB {
			bestFreeMiB = freeMiB
			free = freeMiB / 1024
			total = totalMiB / 1024
			sawLine = true
		}
	}
	if !sawLine {
		return 0, 0, fmt.Errorf("fit: nvidia-smi reported no GPUs")
	}
	return free, total, nil
}

// parseMemField parses one "Name: <kB> kB" line from /proc/meminfo and
// returns the value in GiB. MemAvailable is the kernel's estimate of RAM
// actually usable (after caches and slab); MemTotal is the whole pool.
func parseMemField(meminfo []byte, field string) (float64, error) {
	for _, line := range strings.Split(string(meminfo), "\n") {
		if !strings.HasPrefix(line, field) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return 0, fmt.Errorf("fit: parsing %s %q: %w", strings.TrimSuffix(field, ":"), fields[1], err)
		}
		return kb / 1024 / 1024, nil
	}
	return 0, fmt.Errorf("fit: %s not found in meminfo", strings.TrimSuffix(field, ":"))
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
