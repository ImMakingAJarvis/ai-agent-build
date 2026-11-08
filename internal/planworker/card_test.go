package planworker

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseTaskCardRoundTrip: every Task field the writer emits survives
// taskDescriptionHTML → ParseTaskCard, including HTML-special characters
// and newlines (the writer escapes them; the parser decodes them).
func TestParseTaskCardRoundTrip(t *testing.T) {
	in := Task{
		ID:              "health-endpoint",
		Description:     "Add a /health endpoint that returns 200 OK. It must\nhandle <b>weird & \"quoted\" input</b> and a literal </p> tag.",
		Files:           []string{"app.py", "tests/test_app.py"},
		Dependencies:    []string{"base", "deps-2"},
		AcceptanceCheck: "python3 -m pytest tests/ -q",
		Size:            "S",
		ExecutorModel:   "qwen3.8-27b",
	}
	got, err := ParseTaskCard(taskDescriptionHTML(in))
	if err != nil {
		t.Fatalf("ParseTaskCard: %v", err)
	}
	if got.ID != in.ID {
		t.Errorf("ID = %q, want %q", got.ID, in.ID)
	}
	if got.Title != "" {
		t.Errorf("Title = %q, want empty (the title is the item name)", got.Title)
	}
	if got.Description != in.Description {
		t.Errorf("Description = %q, want %q", got.Description, in.Description)
	}
	if !reflect.DeepEqual(got.Files, in.Files) {
		t.Errorf("Files = %v, want %v", got.Files, in.Files)
	}
	if !reflect.DeepEqual(got.Dependencies, in.Dependencies) {
		t.Errorf("Dependencies = %v, want %v", got.Dependencies, in.Dependencies)
	}
	if got.AcceptanceCheck != in.AcceptanceCheck {
		t.Errorf("AcceptanceCheck = %q, want %q", got.AcceptanceCheck, in.AcceptanceCheck)
	}
	if got.Size != in.Size {
		t.Errorf("Size = %q, want %q", got.Size, in.Size)
	}
	if got.ExecutorModel != in.ExecutorModel {
		t.Errorf("ExecutorModel = %q, want %q", got.ExecutorModel, in.ExecutorModel)
	}
}

// TestParseTaskCardMinimal: Files, Dependencies, Size and ExecutorModel are
// omitted from the description when empty — the parser must accept that
// and leave them zero.
func TestParseTaskCardMinimal(t *testing.T) {
	in := Task{
		ID:              "t1",
		Description:     "Build the base",
		AcceptanceCheck: "go test ./...",
	}
	got, err := ParseTaskCard(taskDescriptionHTML(in))
	if err != nil {
		t.Fatalf("ParseTaskCard: %v", err)
	}
	if got.ID != in.ID || got.Description != in.Description || got.AcceptanceCheck != in.AcceptanceCheck {
		t.Errorf("got %+v", got)
	}
	if len(got.Files) != 0 || len(got.Dependencies) != 0 || got.Size != "" || got.ExecutorModel != "" {
		t.Errorf("optional fields not zero: %+v", got)
	}
}

func TestParseTaskCardRejects(t *testing.T) {
	cases := map[string]string{
		"no marker":        "<p><strong>What to build:</strong> something</p>",
		"no what":          "<p>" + planTaskMarker + "t1</p>",
		"no check":         "<p>" + planTaskMarker + "t1</p>\n<p><strong>What to build:</strong> x</p>",
		"empty description": "<p>" + planTaskMarker + "t1</p>\n<p><strong>What to build:</strong> </p>\n<p><strong>Acceptance check:</strong> x</p>",
	}
	for name, desc := range cases {
		if _, err := ParseTaskCard(desc); err == nil {
			t.Errorf("%s: ParseTaskCard = nil error", name)
		}
	}
}

// TestParseTaskCardEscapedLabel: a description that merely talks about the
// card format cannot fake a second label — the writer escapes its values,
// so only the real tags parse.
func TestParseTaskCardEscapedLabel(t *testing.T) {
	in := Task{
		ID:              "t1",
		Description:     "The card has a <strong>Size:</strong> M line and a plan-task: fake marker.",
		AcceptanceCheck: "true",
		Size:            "S",
	}
	got, err := ParseTaskCard(taskDescriptionHTML(in))
	if err != nil {
		t.Fatalf("ParseTaskCard: %v", err)
	}
	if got.ID != "t1" {
		t.Errorf("ID = %q, want t1 (escaped text must not fake the marker)", got.ID)
	}
	if got.Size != "S" {
		t.Errorf("Size = %q, want S (escaped text must not fake the label)", got.Size)
	}
	if !strings.Contains(got.Description, "plan-task: fake") {
		t.Errorf("Description lost the escaped text: %q", got.Description)
	}
}
