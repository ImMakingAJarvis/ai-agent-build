package models

import (
	"testing"
)

// TestNewClientEngineURLPrecedence is the acceptance test for the
// containerized-stack bug: inside a container the engine is at
// http://engine:11434 (ENGINE_URL), never at localhost. NewClient must
// prefer ENGINE_URL over OLLAMA_URL, as cmd/jarvis-reply and
// internal/slots already do.
func TestNewClientEngineURLPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		engineURL  string
		ollamaURL  string
		wantBase   string
	}{
		{"engine wins over ollama", "http://engine:11434", "http://other:9", "http://engine:11434"},
		{"ollama when no engine", "", "http://other:9", "http://other:9"},
		{"default when neither", "", "", defaultOllamaURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENGINE_URL", tc.engineURL)
			t.Setenv("OLLAMA_URL", tc.ollamaURL)
			if c := NewClient(); c.baseURL != tc.wantBase {
				t.Errorf("baseURL = %q, want %q", c.baseURL, tc.wantBase)
			}
		})
	}
}
