package main

import (
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

func TestPhotoSpecDefaults(t *testing.T) {
	var spec agentjob.Spec
	photoSpecDefaults(&spec, "a cat on a bike")
	if spec.Title != "a cat on a bike" || spec.Instructions != "a cat on a bike" {
		t.Errorf("prompt defaults: %+v", spec)
	}
	if spec.Class != agentjob.ClassFix || spec.Priority != 100 || spec.Driver != "igpu" {
		t.Errorf("photo defaults: %+v", spec)
	}
	if !spec.HasTag("photo") {
		t.Errorf("photo tag missing: %+v", spec.Tags)
	}

	// Explicit fields are kept, and the tag is added at most once.
	spec = agentjob.Spec{Title: "T", Driver: "sonnet", Tags: []string{"photo"}}
	photoSpecDefaults(&spec, "p")
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
}
