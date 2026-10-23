package planner

import (
	"encoding/json"
	"testing"
)

func TestDifficultyToDriver(t *testing.T) {
	tests := []struct {
		difficulty int
		wantDriver string
		wantEffort string
	}{
		{1, "local", "low"},
		{2, "local", "low"},
		{3, "local", "high"},
		{4, "sonnet", "high"},
		{5, "sonnet", "max"},
		{6, "sonnet", "max"}, // out of range treated as max
	}
	for _, tt := range tests {
		driver, effort := DifficultyToDriver(tt.difficulty)
		if driver != tt.wantDriver {
			t.Errorf("difficulty %d: driver = %q, want %q", tt.difficulty, driver, tt.wantDriver)
		}
		if effort != tt.wantEffort {
			t.Errorf("difficulty %d: effort = %q, want %q", tt.difficulty, effort, tt.wantEffort)
		}
	}
}

func TestMapChunkType(t *testing.T) {
	tests := []struct {
		chunkType string
		want      string
	}{
		{"coding", "coding"},
		{"feature", "coding"},
		{"fix", "fix"},
		{"bug", "fix"},
		{"testing", "coding"},
		{"test", "coding"},
		{"config", "background"},
		{"research", "background"},
		{"unknown", "coding"}, // default
	}
	for _, tt := range tests {
		got := mapChunkType(tt.chunkType)
		if got != tt.want {
			t.Errorf("mapChunkType(%q) = %q, want %q", tt.chunkType, got, tt.want)
		}
	}
}

func TestParsePlan(t *testing.T) {
	raw := `{
		"title": "Test Plan",
		"summary": "A test plan",
		"chunks": [
			{
				"id": "chunk-1",
				"title": "First chunk",
				"instructions": "Do something",
				"difficulty": 3,
				"type": "coding",
				"priority": 5
			}
		]
	}`

	plan, err := parsePlan(raw)
	if err != nil {
		t.Fatalf("parsePlan failed: %v", err)
	}
	if plan.Title != "Test Plan" {
		t.Errorf("title = %q, want %q", plan.Title, "Test Plan")
	}
	if len(plan.Chunks) != 1 {
		t.Fatalf("chunks count = %d, want 1", len(plan.Chunks))
	}
	if plan.Chunks[0].Difficulty != 3 {
		t.Errorf("chunk difficulty = %d, want 3", plan.Chunks[0].Difficulty)
	}
}

func TestParsePlanWithMarkdownFences(t *testing.T) {
	raw := "```json\n{\n  \"title\": \"Plan\",\n  \"chunks\": [{\"id\": \"c1\", \"title\": \"T\", \"instructions\": \"I\", \"difficulty\": 2, \"type\": \"coding\", \"priority\": 5}]\n}\n```"

	plan, err := parsePlan(raw)
	if err != nil {
		t.Fatalf("parsePlan with markdown fences failed: %v", err)
	}
	if plan.Title != "Plan" {
		t.Errorf("title = %q, want %q", plan.Title, "Plan")
	}
}

func TestParsePlanEmptyChunks(t *testing.T) {
	raw := `{"title": "Empty", "chunks": []}`
	_, err := parsePlan(raw)
	if err == nil {
		t.Error("expected error for empty chunks, got nil")
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		input    string
		wantOK   bool
	}{
		{`{"a": 1}`, true},
		{`Here is the result: {"a": 1}`, true},
		{"```json\n{\"a\": 1}\n```", true},
		{"no json here", false},
	}
	for _, tt := range tests {
		result := extractJSON(tt.input)
		var v interface{}
		gotOK := json.Unmarshal(result, &v) == nil
		if gotOK != tt.wantOK {
			t.Errorf("extractJSON(%q): unmarshal OK = %v, want %v", tt.input, gotOK, tt.wantOK)
		}
	}
}

func TestBuildChunkInstructions(t *testing.T) {
	plan := &Plan{
		Title:       "Add feature X",
		Summary:     "Implement feature X with tests",
		Constraints: "Must not break existing API",
	}
	chunk := Chunk{
		Title:        "Implement core logic",
		Instructions: "Create file foo.go with function Bar()",
		Difficulty:   3,
		Estimate:     "20 min",
	}

	instructions := buildChunkInstructions(plan, chunk)
	if instructions == "" {
		t.Fatal("expected non-empty instructions")
	}
	if !containsAll(instructions, "Implement core logic", "3/5", "Implement feature X", "Must not break", "foo.go", "20 min") {
		t.Error("instructions missing expected content")
	}
}

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
