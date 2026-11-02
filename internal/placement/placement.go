// Package placement decides where a registry model runs (Phase A5 of
// docs/work-board/PLAN.md, "Model and placement"). The registry pins each
// model to a home location with an optional fallback; Decide picks the
// home, or the fallback when the home is unusable, from live facts: free
// memory, the game watchdog's GPU pause, and which locations the box has
// (Detect). A run:<loc> board label overrides the pin — it must name one
// of the model's own home/fallback locations and be usable right now,
// otherwise the decision is Blocked (the 27B on run:igpu). The package
// also keeps the board's labels in sync (Sync): one model:<id> per enabled
// executor model, one run:<loc> per location the box actually has.
package placement

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// Location is one of the registry's placement values. The values match
// registry.Placements; None is a "no placement" marker, not a location.
type Location string

const (
	GPU   Location = "gpu"
	CPU   Location = "cpu"
	IGPU  Location = "igpu"
	Cloud Location = "cloud"
	None  Location = "none"
)

// BoxLocations are the locations a physical box can have, in stable order
// (logs, label creation). Cloud is not a box location — it is always
// usable because it runs on the provider's hardware.
var BoxLocations = []Location{GPU, CPU, IGPU}

// State is the live state a placement decision needs.
type State struct {
	// FreeVRAMGB and FreeRAMGB are live free memory in GiB (fit.Live
	// reads them: nvidia-smi and MemAvailable in /proc/meminfo).
	FreeVRAMGB float64
	FreeRAMGB  float64
	// GPUPaused is set by the game watchdog while a game holds the GPU.
	GPUPaused bool
	// Present lists the locations this box has (Detect at start). A nil
	// map counts as every location present.
	Present map[Location]bool
}

// Result is the placement decision for one model.
type Result struct {
	Location Location // where to run it ("" when Blocked)
	Blocked  bool
	Reason   string // why Blocked — the words for the Plane comment
}

// Decide picks the location model runs in, from the registry's home and
// fallback locations and the live state s. The caller passes the registry
// entry for the model id (as the bridge does for the fit comment).
//
// override is the run: label's location ("" for none): when set it must
// be one of the model's own home/fallback locations and usable right now,
// or the result is Blocked — an override that can't work is never
// re-routed elsewhere. When empty, the home is tried first, then the
// fallback; the first usable location wins, and a model with no usable
// location is Blocked.
func Decide(m registry.Model, override string, s State) Result {
	var candidates []Location
	for _, l := range []Location{Location(m.HomeLocation), Location(m.FallbackLocation)} {
		if l == None || inList(candidates, l) {
			continue
		}
		candidates = append(candidates, l)
	}
	if len(candidates) == 0 {
		return blocked(fmt.Sprintf("%s has no placement (home_location and fallback_location are both none)", m.ID))
	}

	if override == "" {
		var reasons []string
		for _, loc := range candidates {
			ok, why := usable(m, loc, s)
			if ok {
				return Result{Location: loc}
			}
			reasons = append(reasons, fmt.Sprintf("%s: %s", loc, why))
		}
		return blocked("no usable location for " + m.ID + ": " + strings.Join(reasons, "; "))
	}

	loc := Location(override)
	if !inList(candidates, loc) {
		return blocked(fmt.Sprintf("run:%s can't work: %s is pinned to %s", override, m.ID, join(candidates)))
	}
	if ok, why := usable(m, loc, s); !ok {
		return blocked(fmt.Sprintf("run:%s can't work: %s", override, why))
	}
	return Result{Location: loc}
}

func blocked(why string) Result { return Result{Blocked: true, Reason: why} }

// usable reports whether loc can host model right now.
func usable(m registry.Model, loc Location, s State) (bool, string) {
	if loc == Cloud {
		return true, ""
	}
	if s.Present != nil && !s.Present[loc] {
		return false, "not on this box"
	}
	switch loc {
	case GPU:
		if s.GPUPaused {
			return false, "the GPU is paused by the game watchdog"
		}
		if s.FreeVRAMGB < m.VRAMGB {
			return false, fmt.Sprintf("needs %.1f GiB VRAM, %.1f free", m.VRAMGB, s.FreeVRAMGB)
		}
	case CPU, IGPU:
		// Both draw from system RAM (the iGPU shares it), so both
		// check ram_gb — the registry's CPU-placement requirement.
		if s.FreeRAMGB < m.RAMGB {
			return false, fmt.Sprintf("needs %.1f GiB RAM, %.1f free", m.RAMGB, s.FreeRAMGB)
		}
	}
	return true, ""
}

// Detector probes which locations the box has. The binaries are
// overridable for tests.
type Detector struct {
	SMIBin   string // nvidia-smi (default "nvidia-smi")
	LSPCIBin string // lspci (default "lspci")
}

// Detect reports the locations this box has: gpu when nvidia-smi lists a
// GPU, igpu when lspci shows a display controller that is not the NVIDIA
// one (an AMD or Intel iGPU), cpu always. A probe that fails (missing
// binary) simply reports its location absent.
func (d Detector) Detect(ctx context.Context) map[Location]bool {
	out := map[Location]bool{CPU: true}
	smi := d.SMIBin
	if smi == "" {
		smi = "nvidia-smi"
	}
	if _, err := exec.CommandContext(ctx, smi, "-L").Output(); err == nil {
		out[GPU] = true
	}
	lspci := d.LSPCIBin
	if lspci == "" {
		lspci = "lspci"
	}
	if o, err := exec.CommandContext(ctx, lspci).Output(); err == nil {
		for _, line := range strings.Split(string(o), "\n") {
			lower := strings.ToLower(line)
			isController := strings.Contains(lower, "vga compatible") ||
				strings.Contains(lower, "3d controller") ||
				strings.Contains(lower, "display controller")
			if isController && !strings.Contains(lower, "nvidia") {
				out[IGPU] = true
				break
			}
		}
	}
	return out
}

// Detect runs the default probes.
func Detect(ctx context.Context) map[Location]bool { return Detector{}.Detect(ctx) }

// Names lists the present box locations in BoxLocations order (for logs).
func Names(present map[Location]bool) []string {
	var out []string
	for _, l := range BoxLocations {
		if present[l] {
			out = append(out, string(l))
		}
	}
	return out
}

// ModelPrefix and RunPrefix mark the labels this package owns; Sync
// leaves every other label (epic, plan:, ...) alone.
const (
	ModelPrefix = "model:"
	RunPrefix   = "run:"
)

// Colors new labels get: model: violet, run: amber.
const (
	ModelColor = "#8B5CF6"
	RunColor   = "#F59E0B"
)

// ModelLabel is the board label name for an executor model id.
func ModelLabel(modelID string) string { return ModelPrefix + modelID }

// RunLabel is the board label name for a location override.
func RunLabel(loc Location) string { return RunPrefix + string(loc) }

// Sync makes the model: and run: labels of every project match the
// registry and the box: one model:<id> per enabled executor model, one
// run:<loc> per box location in present (never cloud — a box location is
// where a job physically runs). Labels of either prefix that the registry
// or the box no longer justifies are removed (work items carrying one
// lose the label, as Plane drops the association). It is idempotent — a
// run against an already-synced board changes nothing.
func Sync(ctx context.Context, c *plane.Client, projectIDs []string, f *registry.File, present map[Location]bool, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	executors, err := f.ByRole("executor")
	if err != nil {
		return err
	}
	var wantModel []string
	seen := make(map[string]bool, len(executors))
	for _, m := range executors {
		if !seen[m.ID] {
			seen[m.ID] = true
			wantModel = append(wantModel, ModelLabel(m.ID))
		}
	}
	wantRun := make(map[string]bool, len(BoxLocations))
	for _, loc := range BoxLocations {
		if present[loc] {
			wantRun[RunLabel(loc)] = true
		}
	}
	for _, projectID := range projectIDs {
		labels, err := c.ListLabels(ctx, projectID)
		if err != nil {
			return fmt.Errorf("placement: listing labels: %w", err)
		}
		owned := make(map[string]string, len(labels)) // owned label name → id
		for _, l := range labels {
			if strings.HasPrefix(l.Name, ModelPrefix) || strings.HasPrefix(l.Name, RunPrefix) {
				owned[l.Name] = l.ID
			}
		}
		// Remove labels the registry or the box no longer justifies.
		for name, id := range owned {
			if inList(wantModel, name) || wantRun[name] {
				continue
			}
			if err := c.DeleteLabel(ctx, projectID, id); err != nil {
				return fmt.Errorf("placement: deleting stale label %q: %w", name, err)
			}
			log.Info("executor label removed", "project", projectID, "label", name)
		}
		// Create the missing ones: model: in registry order, then run:
		// in BoxLocations order.
		for _, name := range wantModel {
			if _, ok := owned[name]; ok {
				continue
			}
			if _, err := c.CreateLabel(ctx, projectID, name, ModelColor); err != nil {
				return fmt.Errorf("placement: creating label %q: %w", name, err)
			}
			log.Info("model label added (model enabled as executor)", "project", projectID, "label", name)
		}
		for _, loc := range BoxLocations {
			name := RunLabel(loc)
			if !wantRun[name] {
				continue
			}
			if _, ok := owned[name]; ok {
				continue
			}
			if _, err := c.CreateLabel(ctx, projectID, name, RunColor); err != nil {
				return fmt.Errorf("placement: creating label %q: %w", name, err)
			}
			log.Info("run label added (box has the location)", "project", projectID, "label", name)
		}
	}
	return nil
}

func inList[T comparable](vals []T, v T) bool {
	for _, x := range vals {
		if x == v {
			return true
		}
	}
	return false
}

func join(vals []Location) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = string(v)
	}
	return strings.Join(parts, ", ")
}
