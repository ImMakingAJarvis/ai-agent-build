// Package registry loads and validates the model registry file
// (config/models.yaml). The planner only offers models tagged with the
// "planner" role; see docs/work-board/PLAN.md, "Model registry".
package registry

import (
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Known values from the PLAN.md field table.
var (
	Locations  = []string{"local", "cloud"}
	Engines    = []string{"ollama", "llama.cpp", "vllm", "anthropic", "openai-compatible"}
	Roles      = []string{"planner", "executor", "reviewer"}
	Placements = []string{"gpu", "cpu", "igpu", "cloud", "none"}
)

// Required lists the fields every entry must declare.
var Required = []string{
	"id", "display_name", "location", "engine", "endpoint", "roles",
	"context_tokens", "vram_gb", "ram_gb", "thinking", "cost_per_mtok",
	"enabled", "home_location", "fallback_location",
}

// Model is one entry in the registry.
type Model struct {
	ID               string   `yaml:"id"`
	DisplayName      string   `yaml:"display_name"`
	Location         string   `yaml:"location"`
	Engine           string   `yaml:"engine"`
	Endpoint         string   `yaml:"endpoint"`
	Roles            []string `yaml:"roles"`
	ContextTokens    int      `yaml:"context_tokens"`
	VRAMGB           float64  `yaml:"vram_gb"`
	RAMGB            float64  `yaml:"ram_gb"`
	Thinking         string   `yaml:"thinking"`
	CostPerMTok      float64  `yaml:"cost_per_mtok"`
	Enabled          bool     `yaml:"enabled"`
	HomeLocation     string   `yaml:"home_location"`
	FallbackLocation string   `yaml:"fallback_location"`
}

// File is the root of config/models.yaml.
type File struct {
	Models []Model `yaml:"models"`
}

// Load reads and validates the registry file at path.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse validates a registry document. It first checks that every entry
// declares every required key (typed unmarshalling would silently fill
// zero values for missing fields), then the known values.
func Parse(data []byte) (*File, error) {
	var raw struct {
		Models []map[string]any `yaml:"models"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	for i, m := range raw.Models {
		var missing []string
		for _, k := range Required {
			if _, ok := m[k]; !ok {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			id, _ := m["id"].(string)
			if id == "" {
				id = fmt.Sprintf("entry %d", i+1)
			}
			return nil, fmt.Errorf("model %s: missing field(s): %s", id, strings.Join(missing, ", "))
		}
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Validate checks known values and entry semantics.
func (f *File) Validate() error {
	if len(f.Models) == 0 {
		return fmt.Errorf("registry has no models")
	}
	seen := make(map[string]int, len(f.Models))
	for i, m := range f.Models {
		var problems []string
		if m.ID == "" {
			problems = append(problems, "id is empty")
		}
		if m.DisplayName == "" {
			problems = append(problems, "display_name is empty")
		}
		if m.Endpoint == "" {
			problems = append(problems, "endpoint is empty")
		}
		if !contains(Locations, m.Location) {
			problems = append(problems, fmt.Sprintf("unknown location %q", m.Location))
		}
		if !contains(Engines, m.Engine) {
			problems = append(problems, fmt.Sprintf("unknown engine %q", m.Engine))
		}
		if len(m.Roles) == 0 {
			problems = append(problems, "roles is empty")
		}
		for _, r := range m.Roles {
			if !contains(Roles, r) {
				problems = append(problems, fmt.Sprintf("unknown role %q", r))
			}
		}
		if m.ContextTokens <= 0 {
			problems = append(problems, "context_tokens must be > 0")
		}
		if m.VRAMGB < 0 || m.RAMGB < 0 {
			problems = append(problems, "vram_gb and ram_gb must be >= 0")
		}
		if m.CostPerMTok < 0 {
			problems = append(problems, "cost_per_mtok must be >= 0")
		}
		if !contains(Placements, m.HomeLocation) {
			problems = append(problems, fmt.Sprintf("unknown home_location %q", m.HomeLocation))
		}
		if !contains(Placements, m.FallbackLocation) {
			problems = append(problems, fmt.Sprintf("unknown fallback_location %q", m.FallbackLocation))
		}
		if len(problems) > 0 {
			return fmt.Errorf("model %s: %s", label(m.ID, i), strings.Join(problems, "; "))
		}
		if prev, dup := seen[m.ID]; dup {
			return fmt.Errorf("duplicate model id %q (entries %d and %d)", m.ID, prev+1, i+1)
		}
		seen[m.ID] = i
	}
	return nil
}

func label(id string, i int) string {
	if id == "" {
		return fmt.Sprintf("entry %d", i+1)
	}
	return id
}

func contains(vals []string, v string) bool {
	for _, x := range vals {
		if x == v {
			return true
		}
	}
	return false
}
