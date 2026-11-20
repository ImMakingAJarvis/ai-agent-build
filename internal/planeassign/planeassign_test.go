package planeassign

import "testing"

func TestModelForEmail(t *testing.T) {
	id, ok := ModelForEmail("Jarvis-Bot2@nathan.homes")
	if !ok || id != "qwen3.8-27b" {
		t.Fatalf("qwen = %q %v", id, ok)
	}
	id, ok = ModelForEmail("jarvis-big-brain@nathan.homes")
	if !ok || id != "qwen3.8-flash-next" {
		t.Fatalf("big brain = %q %v", id, ok)
	}
	if _, ok := ModelForEmail("nathan219@gmail.com"); ok {
		t.Fatal("Nathan is not a model")
	}
}
