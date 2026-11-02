package placement

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

// --- Decide --------------------------------------------------------------------

func model(id, home, fallback string, vram, ram float64) registry.Model {
	return registry.Model{ID: id, HomeLocation: home, FallbackLocation: fallback, VRAMGB: vram, RAMGB: ram}
}

// boxState is the real box: 4090 with ~24 GiB free, 128 GiB of RAM with
// ~100 GiB free, all three box locations present, GPU not paused.
var boxState = State{
	FreeVRAMGB: 24, FreeRAMGB: 100,
	Present: map[Location]bool{GPU: true, CPU: true, IGPU: true},
}

func paused(s State) State { s.GPUPaused = true; return s }

func TestDecide(t *testing.T) {
	m27 := model("m27", "gpu", "cpu", 20, 0) // like qwen3.8-27b
	m27NoFB := model("m27nb", "gpu", "none", 20, 0)
	mCloud := model("mcloud", "cloud", "none", 0, 0)
	mIGPU := model("migpu", "igpu", "cpu", 0, 8) // like the iGPU helper model
	mCPU := model("mcpu", "cpu", "none", 0, 16)
	m27RAM := model("m27ram", "gpu", "cpu", 20, 8) // a 27B that could offload to CPU

	cases := []struct {
		name     string
		m        registry.Model
		override string
		state    State
		want     Location
		blocked  bool
	}{
		{"home: gpu model runs on gpu", m27, "", boxState, GPU, false},
		{"fallback when the GPU is paused", m27, "", paused(boxState), CPU, false},
		{"valid override: run:cpu keeps the GPU free", m27, "cpu", boxState, CPU, false},
		{"valid override: run:gpu on a gpu-pinned model", m27, "gpu", boxState, GPU, false},
		{"invalid override: the 27B on run:igpu is blocked", m27, "igpu", boxState, "", true},
		{"home short on VRAM: fallback to cpu", m27, "", State{FreeVRAMGB: 10, FreeRAMGB: 100, Present: boxState.Present}, CPU, false},
		{"override to a paused location is blocked", m27, "gpu", paused(boxState), "", true},
		{"cloud model runs on cloud", mCloud, "", State{}, Cloud, false},
		{"no fallback and home paused: blocked", m27NoFB, "", paused(boxState), "", true},
		{"home location absent: fallback to cpu", mIGPU, "", State{FreeVRAMGB: 24, FreeRAMGB: 100, Present: map[Location]bool{GPU: true, CPU: true}}, CPU, false},
		{"override on an absent location is blocked", mIGPU, "igpu", State{FreeVRAMGB: 24, FreeRAMGB: 100, Present: map[Location]bool{GPU: true, CPU: true}}, "", true},
		{"cpu override short on RAM is blocked", mCPU, "cpu", State{FreeVRAMGB: 24, FreeRAMGB: 8, Present: boxState.Present}, "", true},
		{"iGPU home runs on the iGPU (shared RAM)", mIGPU, "", boxState, IGPU, false},
		{"both home and fallback unusable: blocked", m27RAM, "", State{FreeVRAMGB: 10, FreeRAMGB: 0, Present: map[Location]bool{GPU: true, CPU: true}}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Decide(tc.m, tc.override, tc.state)
			if r.Blocked != tc.blocked {
				t.Fatalf("Blocked = %v (%s), want %v", r.Blocked, r.Reason, tc.blocked)
			}
			if r.Location != tc.want {
				t.Fatalf("Location = %q, want %q", r.Location, tc.want)
			}
			if tc.blocked && r.Reason == "" {
				t.Error("blocked result has no reason")
			}
		})
	}
}

// --- Detect --------------------------------------------------------------------

func writeBin(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return p
}

func sameLocations(t *testing.T, got, want map[Location]bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("locations = %v, want %v", Names(got), Names(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("locations = %v, want %v", Names(got), Names(want))
		}
	}
}

func TestDetectFullBox(t *testing.T) {
	dir := t.TempDir()
	smi := writeBin(t, dir, "smi", "#!/bin/sh\necho 'GPU 0: NVIDIA GeForce RTX 4090 (UUID: x)'\n")
	lspci := writeBin(t, dir, "lspci", "#!/bin/sh\ncat <<'EOF'\n01:00.0 VGA compatible controller: NVIDIA Corporation AD102 [GeForce RTX 4090] (rev a1)\n0c:00.0 VGA compatible controller: Advanced Micro Devices, Inc. [AMD/ATI] Raphael (rev cb)\nEOF\n")
	got := Detector{SMIBin: smi, LSPCIBin: lspci}.Detect(context.Background())
	sameLocations(t, got, map[Location]bool{GPU: true, CPU: true, IGPU: true})
}

func TestDetectNoDiscreteGPU(t *testing.T) {
	// No nvidia-smi → no gpu; the AMD VGA controller still means igpu.
	dir := t.TempDir()
	lspci := writeBin(t, dir, "lspci", "#!/bin/sh\necho '0c:00.0 VGA compatible controller: Advanced Micro Devices, Inc. [AMD/ATI] Raphael (rev cb)'\n")
	got := Detector{SMIBin: filepath.Join(dir, "no-such-smi"), LSPCIBin: lspci}.Detect(context.Background())
	sameLocations(t, got, map[Location]bool{CPU: true, IGPU: true})
}

func TestDetectNoIGPU(t *testing.T) {
	// Only the NVIDIA card in lspci → no igpu.
	dir := t.TempDir()
	smi := writeBin(t, dir, "smi", "#!/bin/sh\necho 'GPU 0: NVIDIA GeForce RTX 4090 (UUID: x)'\n")
	lspci := writeBin(t, dir, "lspci", "#!/bin/sh\necho '01:00.0 VGA compatible controller: NVIDIA Corporation AD102 [GeForce RTX 4090] (rev a1)'\n")
	got := Detector{SMIBin: smi, LSPCIBin: lspci}.Detect(context.Background())
	sameLocations(t, got, map[Location]bool{GPU: true, CPU: true})
}

func TestDetectNoTools(t *testing.T) {
	// Missing binaries are not an error — only cpu remains.
	dir := t.TempDir()
	got := Detector{SMIBin: filepath.Join(dir, "no-smi"), LSPCIBin: filepath.Join(dir, "no-lspci")}.Detect(context.Background())
	sameLocations(t, got, map[Location]bool{CPU: true})
}

// --- Sync ------------------------------------------------------------------------

// fakeLabels is an in-memory stand-in for the v1 project-label endpoints
// (list, create, delete) — all Sync needs. It counts create/delete calls
// so idempotency is observable.
type fakeLabels struct {
	mu      sync.Mutex
	labels  map[string][]map[string]string // projectID → labels
	created int
	deleted int
}

func newFakeLabels() *fakeLabels {
	return &fakeLabels{labels: map[string][]map[string]string{}}
}

func (f *fakeLabels) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "testkey" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rem := strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/testws/projects/")
		parts := strings.Split(strings.Trim(rem, "/"), "/")
		// parts: [pid "labels" (lid)]
		if len(parts) < 2 || parts[1] != "labels" {
			http.NotFound(w, r)
			return
		}
		pid, lid := parts[0], ""
		if len(parts) == 3 {
			lid = parts[2]
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && lid == "":
			out := append([]map[string]string(nil), f.labels[pid]...)
			json.NewEncoder(w).Encode(map[string]any{"results": out, "count": len(out)})
		case r.Method == http.MethodPost && lid == "":
			var body struct {
				Name  string `json:"name"`
				Color string `json:"color"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			l := map[string]string{"id": fmt.Sprintf("lbl-%d", len(f.labels[pid])+1), "name": body.Name, "color": body.Color}
			f.labels[pid] = append(f.labels[pid], l)
			f.created++
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(l)
		case r.Method == http.MethodDelete && lid != "":
			for i, l := range f.labels[pid] {
				if l["id"] == lid {
					f.labels[pid] = append(f.labels[pid][:i], f.labels[pid][i+1:]...)
					break
				}
			}
			f.deleted++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}

func (f *fakeLabels) names(t *testing.T, pid string) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.labels[pid] {
		out = append(out, l["name"])
	}
	return out
}

func (f *fakeLabels) counts() (created, deleted int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created, f.deleted
}

func (f *fakeLabels) seed(pid string, names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range names {
		f.labels[pid] = append(f.labels[pid], map[string]string{"id": fmt.Sprintf("lbl-%s", n), "name": n, "color": "#000000"})
	}
}

func testClient(t *testing.T, f *fakeLabels) *plane.Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := plane.New(plane.Config{BaseURL: srv.URL, APIKey: "testkey", Workspace: "testws"})
	if err != nil {
		t.Fatalf("plane.New: %v", err)
	}
	return c
}

// registryDoc is a minimal valid registry: m-one and m-two are executor
// models (enabled by the flags), m-plan a planner only — it must never
// get a model: label.
func registryDoc(one, two bool) string {
	enabled := func(b bool) string {
		if b {
			return "true"
		}
		return "false"
	}
	entry := func(id string, roles string, en bool) string {
		return fmt.Sprintf(`  - id: %s
    display_name: %s
    location: local
    engine: ollama
    endpoint: http://localhost:1/v1
    roles: [%s]
    context_tokens: 1000
    vram_gb: 1
    ram_gb: 0
    thinking: ""
    cost_per_mtok: 0
    enabled: %s
    home_location: gpu
    fallback_location: cpu
`, id, id, roles, enabled(en))
	}
	return "models:\n" + entry("m-one", "executor", one) + entry("m-two", "executor", two) + entry("m-plan", "planner", true)
}

func parseDoc(t *testing.T, doc string) *registry.File {
	t.Helper()
	f, err := registry.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("registry.Parse: %v", err)
	}
	return f
}

var quietLog = slog.New(slog.DiscardHandler)

var boxPresent = map[Location]bool{GPU: true, CPU: true}

func TestSyncCreatesModelAndRunLabels(t *testing.T) {
	fake := newFakeLabels()
	c := testClient(t, fake)
	ctx := context.Background()

	// Pre-seed: stale labels of both prefixes and two foreign labels.
	fake.seed("p1", "model:m-old", "run:igpu", "epic", "plan:m-plan")

	if err := Sync(ctx, c, []string{"p1"}, parseDoc(t, registryDoc(true, false)), boxPresent, quietLog); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	want := []string{"epic", "plan:m-plan", "model:m-one", "run:gpu", "run:cpu"}
	if got := fake.names(t, "p1"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("labels = %v, want %v (foreign labels must survive)", got, want)
	}
	if created, deleted := fake.counts(); created != 3 || deleted != 2 {
		t.Errorf("counts = %d created / %d deleted, want 3 / 2", created, deleted)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	fake := newFakeLabels()
	c := testClient(t, fake)
	ctx := context.Background()
	f := parseDoc(t, registryDoc(true, true))

	fake.seed("p1", "model:m-one", "model:m-two", "run:gpu", "run:cpu")
	if err := Sync(ctx, c, []string{"p1"}, f, boxPresent, quietLog); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if created, deleted := fake.counts(); created != 0 || deleted != 0 {
		t.Errorf("Sync on an already-synced board changed it: %d created, %d deleted; want 0/0", created, deleted)
	}
}

func TestSyncRemovesModelLabelWhenDisabled(t *testing.T) {
	fake := newFakeLabels()
	c := testClient(t, fake)
	ctx := context.Background()

	fake.seed("p1", "model:m-one", "model:m-two", "run:gpu", "run:cpu")
	// m-two disabled: its model: label goes, m-one's stays.
	if err := Sync(ctx, c, []string{"p1"}, parseDoc(t, registryDoc(true, false)), boxPresent, quietLog); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	want := []string{"model:m-one", "run:gpu", "run:cpu"}
	if got := fake.names(t, "p1"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("labels = %v, want %v", got, want)
	}
}

func TestSyncNeverCreatesRunCloud(t *testing.T) {
	fake := newFakeLabels()
	c := testClient(t, fake)
	ctx := context.Background()

	present := map[Location]bool{GPU: true, CPU: true, IGPU: true, Cloud: true}
	if err := Sync(ctx, c, []string{"p1"}, parseDoc(t, registryDoc(true, false)), present, quietLog); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	got := fake.names(t, "p1")
	for _, n := range got {
		if n == "run:cloud" {
			t.Fatalf("labels = %v, must not include run:cloud", got)
		}
	}
	if len(got) != 4 { // model:m-one + run:gpu + run:cpu + run:igpu
		t.Fatalf("labels = %v, want model:m-one and run:gpu/cpu/igpu", got)
	}
}
