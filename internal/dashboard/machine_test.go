package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photo"
)

func TestSwapState(t *testing.T) {
	cases := []struct {
		name         string
		brainUp      bool
		windowOpen   bool
		windowDetail string
		artModel     string
		wantLabel    string
		wantTone     string
		wantDetail   string
	}{
		{"brain up", true, false, "", "", "brain running", "ok", ""},
		{"brain up wins over stale state file", true, true, "for image.generate", "z-image-turbo", "brain running", "ok", ""},
		{"window with state file and art", false, true, "2026-11-07 22:15 — for image.generate", "z-image-turbo",
			"swap window open", "warn", "2026-11-07 22:15 — for image.generate — z-image-turbo on the 4090"},
		{"window, no state file (art closing)", false, true, "", "z-image-turbo",
			"swap window open", "warn", "brain paused for GPU work — z-image-turbo on the 4090"},
		{"window, no art yet", false, true, "", "",
			"swap window open", "warn", "brain paused for GPU work"},
		{"brain down, no window", false, false, "", "",
			"brain down", "bad", "no swap window open — the watchdog re-arms the brain at the deadline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label, tone, detail := swapState(tc.brainUp, tc.windowOpen, tc.windowDetail, tc.artModel)
			if label != tc.wantLabel || tone != tc.wantTone || detail != tc.wantDetail {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)", label, tone, detail, tc.wantLabel, tc.wantTone, tc.wantDetail)
			}
		})
	}
}

func TestModelDisplayName(t *testing.T) {
	cases := map[string]string{
		"/opt/jarvis-engine/models/jarvis-brain.gguf": "jarvis-brain",
		"/opt/jarvis-igpu/models/jarvis-igpu.gguf":    "jarvis-igpu",
		"qwen2.5:3b":                                  "qwen2.5:3b",
		"jarvis-brain.gguf":                           "jarvis-brain",
	}
	for in, want := range cases {
		if got := modelDisplayName(in); got != want {
			t.Errorf("modelDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseVRAMMetrics(t *testing.T) {
	body := `# HELP nvidia_gpu_mem_used_mb GPU memory used in MB.
nvidia_gpu_mem_used_mb {gpu="0"} 20376
# TYPE nvidia_gpu_mem_used_mb gauge
nvidia_gpu_mem_total_mb {gpu="0"} 24564
nvidia_gpu_temp_celsius {gpu="0"} 42
`
	v := parseVRAMMetrics(body)
	if v == nil {
		t.Fatal("parseVRAMMetrics = nil, want VRAM")
	}
	if v.UsedMB != 20376 || v.TotalMB != 24564 || v.FreeMB() != 24564-20376 {
		t.Errorf("got %+v, want used=20376 total=24564", v)
	}
	if got := parseVRAMMetrics("no gpu metrics here\n"); got != nil {
		t.Errorf("parseVRAMMetrics(no metrics) = %+v, want nil", got)
	}
}

func TestSwapEventDetail(t *testing.T) {
	cases := map[string]string{
		"IN (reason=photo window drained (queue watcher)) ===": "photo window drained (queue watcher)",
		"OUT (maintain=0 jobs=chat-1,chat-2 reason=photo batch (queue watcher)) ===": "photo batch (queue watcher)",
		"IN (reason=none) ===":                                 "",
		"IN (reason=manual --force) ===":                       "manual --force",
	}
	for in, want := range cases {
		if got := swapEventDetail(in); got != want {
			t.Errorf("swapEventDetail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSwapLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swap.log")
	log := `2026-11-07 17:58:12 [jarvis-swap] === swap OUT (maintain=0 jobs=chat-1,chat-2 reason=photo batch (queue watcher)) ===
2026-11-07 17:58:12 [jarvis-swap] stopping brain container jarvis-engine ...
2026-11-07 18:01:37 [jarvis-swap] === swap OUT complete ===
2026-11-07 18:04:38 [jarvis-swap] === swap IN (reason=photo window drained (queue watcher)) ===
2026-11-07 18:04:45 [jarvis-swap] brain is healthy (HTTP 200)
2026-11-07 18:04:46 [jarvis-swap] === swap IN complete ===
`
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	// The last swap *event* is the IN; the OUT-complete/IN-complete lines are
	// not events and must be skipped.
	want := "2026-11-07 18:04 IN: photo window drained (queue watcher)"
	if got := parseSwapLog(path); got != want {
		t.Errorf("parseSwapLog = %q, want %q", got, want)
	}
	if got := parseSwapLog(filepath.Join(dir, "missing.log")); got != "" {
		t.Errorf("parseSwapLog(missing) = %q, want empty", got)
	}
}

func TestSwapWindowState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "swap-window.json")

	if open, _ := swapWindowState(path); open {
		t.Error("swapWindowState(missing) = open, want closed")
	}
	body := `{"for":["image.generate"],"reason":"queued GPU work","opened_at":"2026-11-07T22:15:03Z"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	open, detail := swapWindowState(path)
	if !open || detail != "2026-11-07 22:15 — for image.generate" {
		t.Errorf("swapWindowState = (%v, %q), want (true, %q)", open, detail, "2026-11-07 22:15 — for image.generate")
	}
	// A file that exists but does not parse still counts as open (written
	// early by `out`; a truncated read should read "window", not "down").
	if err := os.WriteFile(path, []byte(`{"for":["image.ge`), 0o644); err != nil {
		t.Fatal(err)
	}
	if open, detail := swapWindowState(path); !open || detail != "" {
		t.Errorf("unparseable file = (%v, %q), want (true, %q)", open, detail, "")
	}
	// No for-list, no opened_at: open with no detail (the panel falls back).
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if open, detail := swapWindowState(path); !open || detail != "" {
		t.Errorf("empty object = (%v, %q), want (true, %q)", open, detail, "")
	}
}

func TestHoldCounts(t *testing.T) {
	jobs := []agentjob.Job{
		{Spec: agentjob.Spec{ID: "a", Tags: []string{photo.Tag}}, State: agentjob.State{Status: agentjob.Ready}},
		{Spec: agentjob.Spec{ID: "b", Tags: []string{photo.Tag}}, State: agentjob.State{Status: agentjob.Ready}},
		{Spec: agentjob.Spec{ID: "c"}, State: agentjob.State{Status: agentjob.Ready}},
		{Spec: agentjob.Spec{ID: "d"}, State: agentjob.State{Status: agentjob.Running}},
		{Spec: agentjob.Spec{ID: "e"}, State: agentjob.State{Status: agentjob.Claimed}},
		{Spec: agentjob.Spec{ID: "f"}, State: agentjob.State{Status: agentjob.Failed}},
		{Spec: agentjob.Spec{ID: "g"}, State: agentjob.State{Status: agentjob.Done}},
		{Spec: agentjob.Spec{ID: "h"}, State: agentjob.State{Status: agentjob.Paused}},
	}
	h := holdCounts(jobs)
	if h.Photo != 2 || h.Ready != 1 || h.Running != 2 || h.Failed != 1 {
		t.Errorf("holdCounts = %+v, want photo=2 ready=1 running=2 failed=1", h)
	}
}

// TestGatherMachine probes the panel against fake engine/art/exporter
// endpoints: everything up, brain healthy, no window, VRAM reported.
func TestGatherMachineAllUp(t *testing.T) {
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"models":[{"name":"/opt/jarvis-engine/models/jarvis-brain.gguf"}]}`))
	}))
	defer brain.Close()
	igpu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"name":"/opt/jarvis-igpu/models/jarvis-igpu.gguf"}]}`))
	}))
	defer igpu.Close()
	art := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Down like it is outside a window: connection refused is the real
		// case, but a 503 stands in for "no model".
		http.Error(w, "no model", http.StatusServiceUnavailable)
	}))
	defer art.Close()
	nvidia := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("nvidia_gpu_mem_used_mb {gpu=\"0\"} 20376\nnvidia_gpu_mem_total_mb {gpu=\"0\"} 24564\n"))
	}))
	defer nvidia.Close()

	conf := t.TempDir()
	s := &Server{
		historyPath: filepath.Join(conf, "jobs.jsonl"),
		engineURL:   brain.URL,
		igpuURL:     igpu.URL,
		artURL:      art.URL,
		nvidiaURL:   nvidia.URL,
		windowPath:  filepath.Join(conf, "swap-window.json"),
		swapLog:     filepath.Join(conf, "swap.log"),
		httpClient:  &http.Client{},
	}

	st := s.gatherMachine()
	if st.Swap != "brain running" || st.SwapTone != "ok" {
		t.Errorf("Swap = %q (%s), want brain running/ok", st.Swap, st.SwapTone)
	}
	if st.VRAM == nil || st.VRAM.UsedMB != 20376 || st.VRAM.TotalMB != 24564 {
		t.Errorf("VRAM = %+v, want 20376/24564", st.VRAM)
	}
	if len(st.Engines) != 3 {
		t.Fatalf("len(Engines) = %d, want 3", len(st.Engines))
	}
	if st.Engines[0].State != "up" || st.Engines[0].Model != "jarvis-brain" {
		t.Errorf("brain card = %+v, want up/jarvis-brain", st.Engines[0])
	}
	if st.Engines[1].State != "up" || st.Engines[1].Model != "jarvis-igpu" {
		t.Errorf("igpu card = %+v, want up/jarvis-igpu", st.Engines[1])
	}
	if st.Engines[2].State != "idle" {
		t.Errorf("art card = %+v, want idle", st.Engines[2])
	}
}

// TestGatherMachinePhotoWindow: brain down, swap window state open, art up
// — the headline must read as a swap window, not a failure.
func TestGatherMachinePhotoWindow(t *testing.T) {
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "refused", http.StatusServiceUnavailable)
	}))
	defer brain.Close()
	art := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","model":"z-image-turbo","loaded":true}`))
	}))
	defer art.Close()

	conf := t.TempDir()
	if err := os.WriteFile(filepath.Join(conf, "swap-window.json"),
		[]byte(`{"for":["image.generate"],"reason":"queued GPU work","opened_at":"2026-11-07T22:15:03Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		historyPath: filepath.Join(conf, "jobs.jsonl"),
		engineURL:   brain.URL,
		igpuURL:     "http://127.0.0.1:1", // down: not part of the assertion
		artURL:      art.URL,
		nvidiaURL:   "http://127.0.0.1:1",
		windowPath:  filepath.Join(conf, "swap-window.json"),
		swapLog:     filepath.Join(conf, "swap.log"),
		httpClient:  &http.Client{},
	}

	st := s.gatherMachine()
	if st.Swap != "swap window open" || st.SwapTone != "warn" {
		t.Errorf("Swap = %q (%s), want swap window open/warn", st.Swap, st.SwapTone)
	}
	if st.SwapDetail != "2026-11-07 22:15 — for image.generate — z-image-turbo on the 4090" {
		t.Errorf("SwapDetail = %q", st.SwapDetail)
	}
	if st.Engines[0].State != "swapped" {
		t.Errorf("brain card state = %q, want swapped", st.Engines[0].State)
	}
	if st.Engines[2].State != "up" || st.Engines[2].Model != "z-image-turbo" {
		t.Errorf("art card = %+v, want up/z-image-turbo", st.Engines[2])
	}
}
