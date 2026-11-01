package registry

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// registryPath is config/models.yaml at the repo root, relative to this package.
func registryPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "config", "models.yaml")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("registry file not found at %s: %v", p, err)
	}
	return p
}

func hasRole(m Model, role string) bool {
	for _, r := range m.Roles {
		if r == role {
			return true
		}
	}
	return false
}

func TestLoadRealRegistry(t *testing.T) {
	f, err := Load(registryPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Models) != 3 {
		t.Fatalf("got %d models, want 3", len(f.Models))
	}
	byID := make(map[string]Model, len(f.Models))
	for _, m := range f.Models {
		byID[m.ID] = m
	}
	if len(byID) != len(f.Models) {
		t.Fatal("registry contains duplicate ids")
	}

	t.Run("qwen3.8-27b", func(t *testing.T) {
		m, ok := byID["qwen3.8-27b"]
		if !ok {
			t.Fatal("missing qwen3.8-27b entry")
		}
		if !m.Enabled {
			t.Error("qwen3.8-27b should be enabled")
		}
		if !hasRole(m, "executor") || !hasRole(m, "planner") {
			t.Errorf("roles = %v, want executor and planner", m.Roles)
		}
		if m.Location != "local" || m.Engine != "ollama" {
			t.Errorf("location/engine = %s/%s, want local/ollama", m.Location, m.Engine)
		}
		if m.ContextTokens != 131072 {
			t.Errorf("context_tokens = %d, want 131072", m.ContextTokens)
		}
		if m.CostPerMTok != 0 {
			t.Errorf("cost_per_mtok = %v, want 0 for a local model", m.CostPerMTok)
		}
	})

	t.Run("qwen3.8-flash-next", func(t *testing.T) {
		m, ok := byID["qwen3.8-flash-next"]
		if !ok {
			t.Fatal("missing qwen3.8-flash-next entry")
		}
		if m.Enabled {
			t.Error("the MoE must stay disabled until it is pulled")
		}
		if len(m.Roles) != 1 || m.Roles[0] != "planner" {
			t.Errorf("roles = %v, want [planner]", m.Roles)
		}
		if m.HomeLocation != "gpu" || m.FallbackLocation != "cpu" {
			t.Errorf("home/fallback = %s/%s, want gpu/cpu", m.HomeLocation, m.FallbackLocation)
		}
	})

	t.Run("claude-sonnet-5-5", func(t *testing.T) {
		m, ok := byID["claude-sonnet-5-5"]
		if !ok {
			t.Fatal("missing claude-sonnet-5-5 entry")
		}
		if !hasRole(m, "planner") || !hasRole(m, "reviewer") {
			t.Errorf("roles = %v, want planner and reviewer", m.Roles)
		}
		if m.Location != "cloud" || m.HomeLocation != "cloud" {
			t.Errorf("location/home = %s/%s, want cloud/cloud", m.Location, m.HomeLocation)
		}
		if m.FallbackLocation != "none" {
			t.Errorf("fallback_location = %s, want none", m.FallbackLocation)
		}
		if m.CostPerMTok <= 0 {
			t.Error("a cloud model must carry a cost")
		}
	})
}

// validDoc declares every required field with known values.
const validDoc = `models:
  - id: a
    display_name: A
    location: local
    engine: ollama
    endpoint: http://localhost:11434/v1
    roles: [executor]
    context_tokens: 1024
    vram_gb: 4
    ram_gb: 0
    thinking: ""
    cost_per_mtok: 0
    enabled: true
    home_location: gpu
    fallback_location: cpu
`

func TestParseAcceptsValidDocument(t *testing.T) {
	f, err := Parse([]byte(validDoc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(f.Models) != 1 || f.Models[0].ID != "a" {
		t.Fatalf("parsed %+v, want one model with id a", f.Models)
	}
}

func TestParseRejectsBadValues(t *testing.T) {
	mutate := func(line, replacement string) string {
		got := strings.Replace(validDoc, line, replacement, 1)
		if got == validDoc {
			t.Fatalf("mutation %q did not apply", line)
		}
		return got
	}
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"unknown role", mutate("    roles: [executor]", "    roles: [wizard]"), "unknown role"},
		{"empty roles", mutate("    roles: [executor]", "    roles: []"), "roles is empty"},
		{"unknown location", mutate("    location: local", "    location: mars"), "unknown location"},
		{"unknown engine", mutate("    engine: ollama", "    engine: telepathy"), "unknown engine"},
		{"unknown home location", mutate("    home_location: gpu", "    home_location: mars"), "unknown home_location"},
		{"unknown fallback location", mutate("    fallback_location: cpu", "    fallback_location: orbit"), "unknown fallback_location"},
		{"zero context tokens", mutate("    context_tokens: 1024", "    context_tokens: 0"), "context_tokens"},
		{"negative vram", mutate("    vram_gb: 4", "    vram_gb: -1"), "vram_gb"},
		{"negative cost", mutate("    cost_per_mtok: 0", "    cost_per_mtok: -2"), "cost_per_mtok"},
		{"empty id", mutate("  - id: a", "  - id: \"\""), "id is empty"},
		{"empty endpoint", mutate("    endpoint: http://localhost:11434/v1", "    endpoint: \"\""), "endpoint is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil {
				t.Fatal("Parse accepted an invalid document")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestParseRejectsMissingRequiredFields(t *testing.T) {
	for _, field := range Required {
		t.Run(field, func(t *testing.T) {
			line := "    " + field
			replace := ""
			if field == "id" { // sequence item: "  - id: a"; keep the dash
				line = "  - id"
				replace = "  -\n"
			}
			re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(line) + `:.*\n`)
			doc := re.ReplaceAllString(validDoc, replace)
			if doc == validDoc {
				t.Fatalf("could not remove field %q", field)
			}
			_, err := Parse([]byte(doc))
			if err == nil {
				t.Fatal("Parse accepted a document missing a required field")
			}
			if !strings.Contains(err.Error(), "missing field") || !strings.Contains(err.Error(), field) {
				t.Fatalf("error %q does not name missing field %q", err, field)
			}
		})
	}
}

func TestParseRejectsDuplicateIDs(t *testing.T) {
	entry := strings.TrimPrefix(validDoc, "models:\n")
	doc := "models:\n" + entry + entry
	_, err := Parse([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "duplicate model id") {
		t.Fatalf("err = %v, want duplicate model id", err)
	}
}

func TestParseRejectsUnparseableYAML(t *testing.T) {
	if _, err := Parse([]byte("models: [unclosed")); err == nil {
		t.Fatal("Parse accepted malformed YAML")
	}
}

func TestValidateRejectsEmptyRegistry(t *testing.T) {
	if _, err := Parse([]byte("models: []")); err == nil {
		t.Fatal("Parse accepted an empty registry")
	}
}
