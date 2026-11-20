package specs

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	yaml "go.yaml.in/yaml/v3"
)

// repoRoot is the module root (this package lives in <root>/specs).
const repoRoot = ".."

func mustAll(t *testing.T) []Spec {
	t.Helper()
	all, err := All()
	if err != nil {
		t.Fatalf("parse specs: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no specs found")
	}
	return all
}

// TestSpecsValid: every spec parses, has its fields and sections, and links
// only to specs that exist.
func TestSpecsValid(t *testing.T) {
	for _, err := range Validate(mustAll(t)) {
		t.Error(err)
	}
}

func composeServices(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	var d struct {
		Services map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(b, &d); err != nil {
		t.Fatalf("parse docker-compose.yml: %v", err)
	}
	var out []string
	for n := range d.Services {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func listDir(t *testing.T, dir string, keep func(os.DirEntry) bool) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoRoot, dir))
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if keep(e) {
			out = append(out, e.Name())
		}
	}
	return out
}

// coverage checks both directions: everything that exists is claimed by a
// spec, and everything a spec claims exists.
func coverage(t *testing.T, what string, exist []string, claimed map[string]string) {
	t.Helper()
	have := map[string]bool{}
	for _, x := range exist {
		have[x] = true
		if _, ok := claimed[x]; !ok {
			t.Errorf("%s %q has no spec — add specs/<id>.md (see specs/README.md) or list it in an existing spec", what, x)
		}
	}
	for x, by := range claimed {
		if !have[x] {
			t.Errorf("%s lists %s %q, which does not exist any more — update the spec", by, what, x)
		}
	}
}

// TestEveryComposeServiceHasSpec: a new compose service cannot land without
// a spec, and a removed one cannot leave a stale spec behind.
func TestEveryComposeServiceHasSpec(t *testing.T) {
	claimed := map[string]string{}
	for _, s := range mustAll(t) {
		for _, c := range s.Compose {
			claimed[c] = s.Path
		}
	}
	coverage(t, "compose service", composeServices(t), claimed)
}

// TestEverySystemdUnitHasSpec: same for the host units in systemd/.
func TestEverySystemdUnitHasSpec(t *testing.T) {
	claimed := map[string]string{}
	for _, s := range mustAll(t) {
		for _, u := range s.Units {
			claimed[u] = s.Path
		}
	}
	units := listDir(t, "systemd", func(e os.DirEntry) bool {
		n := e.Name()
		return !e.IsDir() && (strings.HasSuffix(n, ".service") || strings.HasSuffix(n, ".timer"))
	})
	coverage(t, "systemd unit", units, claimed)
}

// TestEveryCmdHasSpec: every cmd/<name> package is owned by a spec (as
// "cmd/<name>" in binaries). Script binaries a spec lists must exist too.
func TestEveryCmdHasSpec(t *testing.T) {
	claimed := map[string]string{}
	for _, s := range mustAll(t) {
		for _, b := range s.Binaries {
			if strings.HasPrefix(b, "cmd/") {
				claimed[strings.TrimPrefix(b, "cmd/")] = s.Path
				continue
			}
			if _, err := os.Stat(filepath.Join(repoRoot, b)); err != nil {
				t.Errorf("%s lists binary %q, which does not exist", s.Path, b)
			}
		}
	}
	cmds := listDir(t, "cmd", func(e os.DirEntry) bool { return e.IsDir() })
	coverage(t, "cmd package", cmds, claimed)
}

// TestSpecDocsExist: a spec only points at docs that are in the repo.
func TestSpecDocsExist(t *testing.T) {
	for _, s := range mustAll(t) {
		for _, d := range s.Docs {
			if _, err := os.Stat(filepath.Join(repoRoot, d)); err != nil {
				t.Errorf("%s lists doc %q, which does not exist", s.Path, d)
			}
		}
	}
}

// The checker itself.

const goodSpec = `---
id: demo
name: Demo
summary: a demo service
group: plumbing
kind: container
status: active
compose: [demo]
depends_on:
  - {to: other, why: it calls it}
---
## Purpose
p
## Interfaces
i
## Configuration
c
## Data
d
## Deploy
d
## Verify live
v
## Failure modes
f
`

func TestCheckAcceptsGoodSpec(t *testing.T) {
	s, err := Parse("demo.md", []byte(goodSpec))
	if err != nil {
		t.Fatal(err)
	}
	if errs := Check(s); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestCheckRejects(t *testing.T) {
	cases := map[string]string{
		"missing section":  strings.Replace(goodSpec, "## Failure modes\nf\n", "", 1),
		"empty section":    strings.Replace(goodSpec, "## Data\nd\n", "## Data\n\n", 1),
		"unknown group":    strings.Replace(goodSpec, "group: plumbing", "group: nowhere", 1),
		"unknown kind":     strings.Replace(goodSpec, "kind: container", "kind: blob", 1),
		"container no svc": strings.Replace(goodSpec, "compose: [demo]\n", "", 1),
		"edge without why": strings.Replace(goodSpec, "why: it calls it", "why: \"\"", 1),
		"bad health":       strings.Replace(goodSpec, "status: active\n", "status: active\nhealth: ftp://x\n", 1),
		"no summary":       strings.Replace(goodSpec, "summary: a demo service\n", "", 1),
	}
	for name, src := range cases {
		s, err := Parse("demo.md", []byte(src))
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if len(Check(s)) == 0 {
			t.Errorf("%s: Check accepted a bad spec", name)
		}
	}
	if _, err := Parse("demo.md", []byte("no front matter")); err == nil {
		t.Error("Parse accepted a file without front matter")
	}
	if _, err := Parse("demo.md", []byte(strings.Replace(goodSpec, "name: Demo", "name: Demo\nbogus: 1", 1))); err == nil {
		t.Error("Parse accepted an unknown front matter field")
	}
}

func TestValidateLinks(t *testing.T) {
	fsys := fstest.MapFS{"demo.md": {Data: []byte(goodSpec)}}
	all, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	errs := Validate(all)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), `depends_on "other" is not a spec id`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("dangling depends_on not reported: %v", errs)
	}
}

func TestSourceRejectsPaths(t *testing.T) {
	if _, err := Source("../go.mod"); err == nil {
		t.Fatal("Source read outside the specs dir")
	}
}
