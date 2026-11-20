// Package specs holds the service specs: one Markdown file per service,
// component or external system the box depends on, each with a YAML front
// matter block (the machine-readable part) and a fixed set of body sections
// (the human part). See specs/README.md for the standard.
//
// The files are embedded, so every binary that imports this package carries
// the specs of the commit it was built from — the dashboard's system map
// (/map) renders from them and can never drift from the deployed code.
//
// specs_test.go is the enforcement: `go test ./...` fails when a compose
// service, a systemd unit or a cmd/ package has no spec, when a spec names a
// service/unit/package that no longer exists, or when a spec is missing a
// required field or section.
package specs

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

//go:embed *.md
var files embed.FS

// Edge is one dependency: this spec's service calls, reads or needs To.
type Edge struct {
	To  string `yaml:"to" json:"to"`
	Why string `yaml:"why" json:"why"`
}

// Spec is one parsed spec file.
type Spec struct {
	ID        string   `yaml:"id" json:"id"`
	Name      string   `yaml:"name" json:"name"`
	Summary   string   `yaml:"summary" json:"summary"`
	Group     string   `yaml:"group" json:"group"`
	Kind      string   `yaml:"kind" json:"kind"`
	Status    string   `yaml:"status" json:"status"`
	Compose   []string `yaml:"compose" json:"compose,omitempty"`
	Units     []string `yaml:"units" json:"units,omitempty"`
	Binaries  []string `yaml:"binaries" json:"binaries,omitempty"`
	Ports     []string `yaml:"ports" json:"ports,omitempty"`
	Health    string   `yaml:"health" json:"health,omitempty"`
	Docs      []string `yaml:"docs" json:"docs,omitempty"`
	DependsOn []Edge   `yaml:"depends_on" json:"depends_on,omitempty"`

	// Path is the spec's file name inside specs/; Body is the Markdown after
	// the front matter.
	Path string `yaml:"-" json:"path"`
	Body string `yaml:"-" json:"-"`
}

// Groups are the map's lanes, in display order (left to right).
var Groups = []string{
	"external", "face", "agent", "board", "plumbing", "resources",
	"capability", "brain", "media", "house", "memory", "monitoring",
}

// Kinds says how a spec's thing runs.
var Kinds = []string{
	"container",    // a docker compose service
	"host-service", // a long-running host systemd unit
	"timer",        // a systemd timer + oneshot
	"cli",          // run on demand (by the agent, a script or a person)
	"external",     // another box or a cloud service
}

// Statuses says whether it runs today.
var Statuses = []string{
	"active",   // runs today
	"optional", // defined, started on demand (a compose profile, a window)
	"disabled", // installed but off on purpose
	"external", // not on this box
}

// Sections are the body headings every spec must have, in this order, each
// with at least one non-blank line under it.
var Sections = []string{
	"Purpose",
	"Interfaces",
	"Configuration",
	"Data",
	"Deploy",
	"Verify live",
	"Failure modes",
}

// specFile reports whether name is a spec (README/template files are not).
func specFile(name string) bool {
	return strings.HasSuffix(name, ".md") && !strings.HasPrefix(name, "_") &&
		name != "README.md"
}

// All parses every embedded spec, sorted by id.
func All() ([]Spec, error) {
	return Load(files)
}

// Load parses every spec file in fsys (top level only).
func Load(fsys fs.FS) ([]Spec, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []Spec
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !specFile(e.Name()) {
			continue
		}
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s, err := Parse(e.Name(), b)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, errors.Join(errs...)
}

// Parse reads one spec: a `---` YAML front matter block, then the body.
func Parse(name string, b []byte) (Spec, error) {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(b, []byte("---\n")) {
		return Spec{}, fmt.Errorf("%s: no front matter (the file must start with ---)", name)
	}
	rest := b[len("---\n"):]
	end := bytes.Index(rest, []byte("\n---\n"))
	if end < 0 {
		return Spec{}, fmt.Errorf("%s: front matter is not closed with ---", name)
	}
	var s Spec
	dec := yaml.NewDecoder(bytes.NewReader(rest[:end]))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("%s: front matter: %w", name, err)
	}
	s.Path = name
	s.Body = string(rest[end+len("\n---\n"):])
	return s, nil
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Check validates one spec on its own: required fields, known values and
// the required body sections.
func Check(s Spec) []error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{s.Path}, a...)...))
	}
	if !idRe.MatchString(s.ID) {
		bad("id %q must be lowercase letters, digits and dashes", s.ID)
	}
	if want := s.ID + ".md"; s.Path != "" && s.Path != want {
		bad("file name must be %s (the id)", want)
	}
	if strings.TrimSpace(s.Name) == "" {
		bad("name is empty")
	}
	if strings.TrimSpace(s.Summary) == "" {
		bad("summary is empty (one line: what it is for)")
	}
	if !contains(Groups, s.Group) {
		bad("group %q is not one of %v", s.Group, Groups)
	}
	if !contains(Kinds, s.Kind) {
		bad("kind %q is not one of %v", s.Kind, Kinds)
	}
	if !contains(Statuses, s.Status) {
		bad("status %q is not one of %v", s.Status, Statuses)
	}
	if s.Kind == "container" && len(s.Compose) == 0 {
		bad("kind container but no compose services listed")
	}
	if (s.Kind == "host-service" || s.Kind == "timer") && len(s.Units) == 0 {
		bad("kind %s but no systemd units listed", s.Kind)
	}
	if s.Kind == "external" && s.Status != "external" {
		bad("kind external must have status external")
	}
	if h := s.Health; h != "" && !strings.HasPrefix(h, "http://") && !strings.HasPrefix(h, "tcp://") {
		bad("health %q must be an http:// URL or tcp://host:port", h)
	}
	for _, e := range s.DependsOn {
		if strings.TrimSpace(e.Why) == "" {
			bad("depends_on %q has no why", e.To)
		}
	}
	for _, sec := range missingSections(s.Body) {
		bad("missing or empty section %q", "## "+sec)
	}
	return errs
}

// missingSections returns the required sections the body lacks (or has with
// nothing under them).
func missingSections(body string) []string {
	content := map[string]bool{}
	cur := ""
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "## ") {
			cur = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			if _, ok := content[cur]; !ok {
				content[cur] = false
			}
			continue
		}
		if cur != "" && strings.TrimSpace(line) != "" {
			content[cur] = true
		}
	}
	var missing []string
	for _, sec := range Sections {
		if !content[sec] {
			missing = append(missing, sec)
		}
	}
	return missing
}

// Validate checks every spec and the links between them: unique ids, no
// compose service / unit / binary claimed twice, every depends_on target is
// a spec.
func Validate(all []Spec) []error {
	var errs []error
	ids := map[string]string{}
	for _, s := range all {
		errs = append(errs, Check(s)...)
		if prev, ok := ids[s.ID]; ok {
			errs = append(errs, fmt.Errorf("id %q is used by both %s and %s", s.ID, prev, s.Path))
		}
		ids[s.ID] = s.Path
	}
	claimed := map[string]string{}
	claim := func(kind, v, by string) {
		key := kind + ":" + v
		if prev, ok := claimed[key]; ok && prev != by {
			errs = append(errs, fmt.Errorf("%s %q is claimed by both %s and %s", kind, v, prev, by))
		}
		claimed[key] = by
	}
	for _, s := range all {
		for _, c := range s.Compose {
			claim("compose service", c, s.Path)
		}
		for _, u := range s.Units {
			claim("systemd unit", u, s.Path)
		}
		for _, b := range s.Binaries {
			claim("binary", b, s.Path)
		}
		for _, e := range s.DependsOn {
			if _, ok := ids[e.To]; !ok {
				errs = append(errs, fmt.Errorf("%s: depends_on %q is not a spec id", s.Path, e.To))
			}
			if e.To == s.ID {
				errs = append(errs, fmt.Errorf("%s: depends on itself", s.Path))
			}
		}
	}
	return errs
}

// Source returns the raw Markdown of one embedded spec by id.
func Source(id string) ([]byte, error) {
	if !idRe.MatchString(id) {
		return nil, fs.ErrNotExist
	}
	return files.ReadFile(id + ".md")
}
