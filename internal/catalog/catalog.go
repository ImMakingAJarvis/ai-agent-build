// Package catalog loads and validates the capability catalog
// (config/capabilities.yaml) — the single source of truth for where a
// capability can run, what it costs, and how long it takes. jarvis-mq
// caps renders it for the model (one line per capability: where, cost,
// queue depth, typical runtime), resourced (task 26) will budget it, and
// the brain's round preamble will read it. Plan §3.5, task EVB-19.
//
// A capability names one or more placements (workers): the same
// capability may run in several places, each with its own model,
// footprint and start/stop. needs_brain_out marks the placements that
// need the 4090's VRAM free of the brain — those run in the brain's swap
// window (docs/brain-swap.md); the rest run alongside it.
package catalog

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Locations a worker may run on (the plan's device vocabulary).
var Locations = []string{"gpu", "igpu", "cpu"}

// RemotePrefix marks off-box placements ("remote:<host>"), matched by
// prefix rather than a fixed list.
const RemotePrefix = "remote:"

// Required lists the fields every worker entry must declare.
var Required = []string{
	"location", "model", "vram_mb", "ram_mb", "needs_brain_out",
	"start", "stop", "typical_runtime", "idle_s",
}

// Worker is one placement of a capability.
type Worker struct {
	Location       string `yaml:"location"` // gpu | igpu | cpu | remote:<host>
	Model          string `yaml:"model"`    // model id from config/models.yaml
	VRAMMB         int    `yaml:"vram_mb"`  // VRAM held while loaded (0 = none)
	RAMMB          int    `yaml:"ram_mb"`   // host RAM held
	NeedsBrainOut  bool   `yaml:"needs_brain_out"` // needs the 4090 free of the brain (swap window)
	Start          string `yaml:"start"`  // command that brings the placement up ("" = always on)
	Stop           string `yaml:"stop"`   // command that frees it again
	TypicalRuntime string `yaml:"typical_runtime"` // Go duration, e.g. "45s"
	IdleS          int    `yaml:"idle_s"` // unload after this many idle seconds (0 = never)
}

// Runtime parses typical_runtime.
func (w Worker) Runtime() (time.Duration, error) {
	return time.ParseDuration(w.TypicalRuntime)
}

// Cost renders a worker's cost the way caps shows it to the model — the
// two questions are "does this need the 4090 out of the brain" and "can
// it run while the brain is in a conversation".
func (w Worker) Cost() string {
	if w.NeedsBrainOut {
		return "needs brain out"
	}
	switch {
	case w.Location == "cpu":
		return "runs alongside you on cpu"
	case w.Location == "igpu":
		return "runs alongside you on igpu"
	case strings.HasPrefix(w.Location, RemotePrefix):
		return "runs off-box"
	default:
		// gpu without needs_brain_out: co-scheduled in the free VRAM
		// the brain leaves behind (~6 GB idle).
		return "runs alongside you in free VRAM"
	}
}

// Capability is one entry of the catalog.
type Capability struct {
	Name        string   `yaml:"-"` // the map key, not a YAML field
	Description string   `yaml:"description"`
	Workers     []Worker `yaml:"workers"`
}

// Locations is the distinct worker locations, in file order.
func (c Capability) Locations() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, w := range c.Workers {
		if !seen[w.Location] {
			seen[w.Location] = true
			out = append(out, w.Location)
		}
	}
	return out
}

// WorkerAt returns the capability's placement at location (the first
// worker entry with that location), or nil when it has none there.
func (c Capability) WorkerAt(location string) *Worker {
	for i, w := range c.Workers {
		if w.Location == location {
			return &c.Workers[i]
		}
	}
	return nil
}

// Cost renders the capability's cost: one placement's cost, or
// "location:cost" pairs joined by " | " when it can run in several
// places.
func (c Capability) Cost() string {
	if len(c.Workers) == 1 {
		return c.Workers[0].Cost()
	}
	parts := make([]string, len(c.Workers))
	for i, w := range c.Workers {
		parts[i] = w.Location + ":" + w.Cost()
	}
	return strings.Join(parts, " | ")
}

// TypicalRuntime is the fastest placement's typical_runtime — the number
// the model waits against, since the broker hands the request to the best
// free placement.
func (c Capability) TypicalRuntime() (time.Duration, error) {
	if len(c.Workers) == 0 {
		return 0, fmt.Errorf("capability %s has no workers", c.Name)
	}
	best := time.Duration(0)
	for i, w := range c.Workers {
		d, err := w.Runtime()
		if err != nil {
			return 0, fmt.Errorf("capability %s: worker %d: %w", c.Name, i+1, err)
		}
		if i == 0 || d < best {
			best = d
		}
	}
	return best, nil
}

// File is the root of config/capabilities.yaml: capability name → entry.
type File struct {
	// Capabilities maps a capability name (queue cap.<name>, events
	// job.<name>.*) to its placements.
	Capabilities map[string]Capability `yaml:"capabilities"`
}

// SortedNames lists the capability names in catalog order (sorted, so
// jarvis-mq caps output is stable).
func (f *File) SortedNames() []string {
	names := make([]string, 0, len(f.Capabilities))
	for name := range f.Capabilities {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ByName returns the capability named cap, or nil.
func (f *File) ByName(cap string) *Capability {
	c, ok := f.Capabilities[cap]
	if !ok {
		return nil
	}
	return &c
}

// Load reads and validates the catalog file at path.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse validates a catalog document. It first checks that every worker
// declares every required key (typed unmarshalling would silently fill
// zero values for missing fields), then the known values — the same
// pattern as internal/registry.
func Parse(data []byte) (*File, error) {
	var raw struct {
		Capabilities map[string]map[string]any `yaml:"capabilities"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse capabilities: %w", err)
	}
	for name, body := range raw.Capabilities {
		workers, ok := body["workers"].([]any)
		if !ok || len(workers) == 0 {
			return nil, fmt.Errorf("capability %s: needs a non-empty workers list", name)
		}
		for i, wk := range workers {
			worker, ok := wk.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("capability %s: worker %d is not a mapping", name, i+1)
			}
			var missing []string
			for _, k := range Required {
				if _, ok := worker[k]; !ok {
					missing = append(missing, k)
				}
			}
			if len(missing) > 0 {
				return nil, fmt.Errorf("capability %s: worker %d missing field(s): %s", name, i+1, strings.Join(missing, ", "))
			}
		}
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse capabilities: %w", err)
	}
	for name, c := range f.Capabilities {
		c.Name = name
		f.Capabilities[name] = c
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Validate checks known values and entry semantics.
func (f *File) Validate() error {
	if len(f.Capabilities) == 0 {
		return fmt.Errorf("catalog has no capabilities")
	}
	for _, name := range f.SortedNames() {
		c := f.Capabilities[name]
		var problems []string
		if !isCapName(name) {
			problems = append(problems, fmt.Sprintf("name %q is not a valid capability name (lowercase dot-separated, e.g. image.generate)", name))
		}
		if len(c.Workers) == 0 {
			problems = append(problems, "no workers")
		}
		for i, w := range c.Workers {
			wp := w.validate()
			for _, p := range wp {
				problems = append(problems, fmt.Sprintf("worker %d: %s", i+1, p))
			}
		}
		if len(problems) > 0 {
			return fmt.Errorf("capability %s: %s", name, strings.Join(problems, "; "))
		}
	}
	return nil
}

func (w Worker) validate() []string {
	var problems []string
	if !validLocation(w.Location) {
		problems = append(problems, fmt.Sprintf("unknown location %q (want %s or %s<host>)", w.Location, strings.Join(Locations, "|"), RemotePrefix))
	}
	if w.Model == "" {
		problems = append(problems, "model is empty (a model id from config/models.yaml)")
	}
	if w.VRAMMB < 0 || w.RAMMB < 0 {
		problems = append(problems, "vram_mb and ram_mb must be >= 0")
	}
	if _, err := time.ParseDuration(w.TypicalRuntime); err != nil {
		problems = append(problems, fmt.Sprintf("typical_runtime %q is not a Go duration (e.g. 45s)", w.TypicalRuntime))
	}
	if w.IdleS < 0 {
		problems = append(problems, "idle_s must be >= 0")
	}
	return problems
}

func validLocation(loc string) bool {
	if strings.HasPrefix(loc, RemotePrefix) && len(loc) > len(RemotePrefix) {
		return true
	}
	for _, l := range Locations {
		if loc == l {
			return true
		}
	}
	return false
}

// isCapName accepts the queue/exchange vocabulary: lowercase words
// joined by dots (echo, image.generate). A queue name with a "/" or an
// uppercase letter would be legal AMQP but would not match the
// cap.<name> routing used everywhere else.
func isCapName(name string) bool {
	if name == "" {
		return false
	}
	for _, part := range strings.Split(name, ".") {
		if part == "" {
			return false
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}
