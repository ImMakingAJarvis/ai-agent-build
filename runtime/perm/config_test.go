package perm

import (
	"reflect"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/runtime/config"
)

func TestFromConfigCopiesRules(t *testing.T) {
	var c config.Config
	c.Permissions.Allow = []config.PermRule{{Tool: "read_file", Pattern: "*.md"}}
	c.Permissions.Deny = []config.PermRule{
		{Tool: "shell", Pattern: "rm*"},
		{Tool: "write_file", Pattern: "secret*"},
	}
	r := FromConfig(c)

	if want := []Rule{{Tool: "read_file", Pattern: "*.md"}}; !reflect.DeepEqual(r.Allow, want) {
		t.Errorf("FromConfig Allow = %v, want %v", r.Allow, want)
	}
	wantDeny := []Rule{
		{Tool: "shell", Pattern: "rm*"},
		{Tool: "write_file", Pattern: "secret*"},
	}
	if !reflect.DeepEqual(r.Deny, wantDeny) {
		t.Errorf("FromConfig Deny = %v, want %v", r.Deny, wantDeny)
	}

	// The copy is independent: mutating the result must not touch the
	// config...
	r.Allow[0].Tool = "mutated"
	if got := c.Permissions.Allow[0].Tool; got != "read_file" {
		t.Errorf("mutating the result changed the config: %q, want %q", got, "read_file")
	}
	// ...and mutating the config must not touch the result.
	c.Permissions.Deny[0].Pattern = "changed"
	if got := r.Deny[0].Pattern; got != "rm*" {
		t.Errorf("mutating the config changed the result: %q, want %q", got, "rm*")
	}
}

func TestFromConfigEmptyIsEmpty(t *testing.T) {
	var c config.Config
	r := FromConfig(c)
	if len(r.Allow) != 0 || len(r.Deny) != 0 {
		t.Errorf("FromConfig(empty config) = Allow %d, Deny %d, want 0 and 0", len(r.Allow), len(r.Deny))
	}
}
