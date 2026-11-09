package main

import (
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photo"
)

func TestPhotoSpecDefaults(t *testing.T) {
	dir := t.TempDir()
	spec := agentjob.Spec{ID: "img-1-ab"}
	if err := photoSpecDefaults(&spec, dir, "a cat on a bike"); err != nil {
		t.Fatalf("photoSpecDefaults: %v", err)
	}
	if spec.Title != "a cat on a bike" || spec.Instructions != "a cat on a bike" {
		t.Errorf("prompt defaults: %+v", spec)
	}
	if spec.Class != agentjob.ClassFix || spec.Priority != 100 || spec.Driver != "igpu" {
		t.Errorf("photo defaults: %+v", spec)
	}
	if !spec.HasTag("photo") {
		t.Errorf("photo tag missing: %+v", spec.Tags)
	}
	// The executor reads the prompt from the sidecar: it must exist before
	// the job lands ready.
	if got, err := photo.ReadPrompt(dir, "img-1-ab"); err != nil || got != "a cat on a bike" {
		t.Errorf("prompt sidecar: got %q err %v", got, err)
	}

	// Explicit fields are kept, and the tag is added at most once.
	spec = agentjob.Spec{ID: "img-2-ab", Title: "T", Driver: "sonnet", Tags: []string{"photo"}}
	if err := photoSpecDefaults(&spec, dir, "p"); err != nil {
		t.Fatalf("photoSpecDefaults: %v", err)
	}
	if spec.Driver != "sonnet" || spec.Title != "T" {
		t.Errorf("explicit fields clobbered: %+v", spec)
	}
	n := 0
	for _, tg := range spec.Tags {
		if tg == "photo" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("photo tag added twice: %+v", spec.Tags)
	}

	// No prompt: no sidecar to write (and no error).
	spec = agentjob.Spec{ID: "img-3-ab"}
	if err := photoSpecDefaults(&spec, dir, ""); err != nil {
		t.Fatalf("photoSpecDefaults with empty prompt: %v", err)
	}
	if _, err := photo.ReadPrompt(dir, "img-3-ab"); err == nil {
		t.Errorf("unexpected prompt sidecar for a job without a prompt")
	}
}
