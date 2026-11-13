package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

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
nvidia_gpu_power_w {gpu="0"} 352.4
nvidia_gpu_temp_celsius {gpu="0"} 42
`
	v := parseVRAMMetrics(body)
	if v == nil {
		t.Fatal("parseVRAMMetrics = nil, want VRAM")
	}
	if v.UsedMB != 20376 || v.TotalMB != 24564 || v.FreeMB() != 24564-20376 {
		t.Errorf("got %+v, want used=20376 total=24564", v)
	}
	if v.PowerW != 352.4 {
		t.Errorf("PowerW = %v, want 352.4", v.PowerW)
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

// TestProbeResourced: the broker's /state becomes the Machine view's
// window card, the per-device lease list and the capability queue
// depths (EVB-30) — grouped, sorted, and with the in-flight depth from
// the snapshot.
func TestProbeResourced(t *testing.T) {
	state := `{
	  "leases": [
	    {"holder":"brain","device":"gpu","vram_mb":17600,"since":"2026-11-12T10:00:00Z"},
	    {"holder":"capworker:image.generate","capability":"image.generate","device":"gpu","vram_mb":12288,"since":"2026-11-12T10:05:00Z"},
	    {"holder":"capworker:llm.small","capability":"llm.small","device":"igpu","vram_mb":5500,"since":"2026-11-12T10:06:00Z"}
	  ],
	  "window": {
	    "open": true,
	    "for": ["image.generate"],
	    "opened_at": "2026-11-12T10:04:00Z",
	    "reason": "queued work on image.generate with the brain idle",
	    "cap_depths": {"image.generate": 1, "llm.small": 0}
	  }
	}`
	res := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/state" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(state))
	}))
	defer res.Close()

	s := &Server{resourcedURL: res.URL, httpClient: &http.Client{}}
	p := s.probeResourced()
	if p == nil {
		t.Fatal("probeResourced = nil")
	}
	if !p.Window.Open || len(p.Window.For) != 1 || p.Window.For[0] != "image.generate" {
		t.Errorf("window = %+v, want open for image.generate", p.Window)
	}
	if p.Window.Reason != "queued work on image.generate with the brain idle" {
		t.Errorf("window reason = %q", p.Window.Reason)
	}
	if p.Window.Age == "" {
		t.Error("open window must carry an age")
	}
	if len(p.Leases) != 2 || p.Leases[0].Device != "gpu" || len(p.Leases[0].Leases) != 2 ||
		p.Leases[1].Device != "igpu" || len(p.Leases[1].Leases) != 1 {
		t.Fatalf("leases = %+v, want gpu(2) then igpu(1)", p.Leases)
	}
	if p.Leases[0].Leases[0].Holder != "brain" || p.Leases[0].Leases[1].Capability != "image.generate" {
		t.Errorf("gpu leases = %+v", p.Leases[0].Leases)
	}
	if len(p.CapDepths) != 2 || p.CapDepths[0].Cap != "image.generate" || p.CapDepths[0].Depth != 1 ||
		p.CapDepths[1].Cap != "llm.small" || p.CapDepths[1].Depth != 0 {
		t.Errorf("cap depths = %+v, want image.generate:1 then llm.small:0", p.CapDepths)
	}

	// A closed window still reports the last decision's reason.
	res2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"window":{"open":false,"reason":"drained (all capability queues empty)"}}`))
	}))
	defer res2.Close()
	s2 := &Server{resourcedURL: res2.URL, httpClient: &http.Client{}}
	p2 := s2.probeResourced()
	if p2 == nil || p2.Window.Open || !p2.Window.Up ||
		p2.Window.Reason != "drained (all capability queues empty)" {
		t.Errorf("closed window = %+v, want up/closed with the last reason", p2)
	}

	// resourced down: the probe degrades to nil, not an error.
	s3 := &Server{resourcedURL: "http://127.0.0.1:1", httpClient: &http.Client{}}
	if got := s3.probeResourced(); got != nil {
		t.Errorf("probeResourced(down) = %+v, want nil", got)
	}
}

// TestWaitingBrainJobs: only the waiting LOCAL jobs show, with their
// wait age and what they wait on; a non-local driver and a ready job
// are not brain waits.
func TestWaitingBrainJobs(t *testing.T) {
	now := time.Now()
	since := now.Add(-45 * time.Second).UTC().Format(time.RFC3339)
	jobs := []agentjob.Job{
		{Spec: agentjob.Spec{ID: "j1", Title: "make a photo", WaitOn: []string{"j1-img-1"}},
			State: agentjob.State{Status: agentjob.Waiting, WaitingSince: since, WaitNote: "image in flight"}},
		{Spec: agentjob.Spec{ID: "j2", Driver: "sonnet", WaitOn: []string{"x"}},
			State: agentjob.State{Status: agentjob.Waiting, WaitingSince: since}},
		{Spec: agentjob.Spec{ID: "j3"}, State: agentjob.State{Status: agentjob.Ready}},
	}
	out := waitingBrainJobs(jobs, now)
	if len(out) != 1 || out[0].ID != "j1" {
		t.Fatalf("waitingBrainJobs = %+v, want only j1", out)
	}
	if out[0].WaitAge != "45s" || out[0].WaitNote != "image in flight" {
		t.Errorf("j1 row = %+v, want waited 45s with the note", out[0])
	}
	if len(out[0].WaitOn) != 1 || out[0].WaitOn[0] != "j1-img-1" {
		t.Errorf("j1 wait_on = %v", out[0].WaitOn)
	}
	// A waiting job with no WaitOn renders a placeholder, not nothing.
	blank := waitingBrainJobs([]agentjob.Job{
		{Spec: agentjob.Spec{ID: "j9"}, State: agentjob.State{Status: agentjob.Waiting}},
	}, now)
	if len(blank) != 1 || len(blank[0].WaitOn) != 1 || blank[0].WaitOn[0] != "—" {
		t.Errorf("no-wait_on job = %+v, want the — placeholder", blank)
	}
}

// TestGatherMachineWithResourced: the broker's window view wins over the
// state file — the headline shows the broker's reason and the window
// card carries the open state (EVB-30).
func TestGatherMachineWithResourced(t *testing.T) {
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "refused", http.StatusServiceUnavailable)
	}))
	defer brain.Close()
	res := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/state" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"leases":[],"window":{"open":true,"for":["image.generate"],"opened_at":"2026-11-12T10:04:00Z","reason":"queued work","cap_depths":{"image.generate":1}}}`))
	}))
	defer res.Close()

	conf := t.TempDir()
	s := &Server{
		historyPath:  filepath.Join(conf, "jobs.jsonl"),
		engineURL:    brain.URL,
		igpuURL:      "http://127.0.0.1:1",
		artURL:       "http://127.0.0.1:1",
		nvidiaURL:    "http://127.0.0.1:1",
		resourcedURL: res.URL,
		windowPath:   filepath.Join(conf, "swap-window.json"), // no file: broker view alone
		swapLog:      filepath.Join(conf, "swap.log"),
		httpClient:   &http.Client{},
	}

	st := s.gatherMachine()
	if st.Swap != "swap window open" || st.SwapTone != "warn" {
		t.Errorf("Swap = %q (%s), want swap window open/warn", st.Swap, st.SwapTone)
	}
	if st.SwapDetail != "for image.generate — queued work" {
		t.Errorf("SwapDetail = %q, want the broker's for+reason", st.SwapDetail)
	}
	if st.Window == nil || !st.Window.Open || st.Window.Age == "" {
		t.Errorf("Window = %+v, want open with an age", st.Window)
	}
	if len(st.CapDepths) != 1 || st.CapDepths[0].Cap != "image.generate" || st.CapDepths[0].Depth != 1 {
		t.Errorf("CapDepths = %+v", st.CapDepths)
	}
}

func TestShortDur(t *testing.T) {
	cases := map[time.Duration]string{
		-5 * time.Second: "0s",
		45 * time.Second: "45s",
		12 * time.Minute: "12m",
		2*time.Hour + 3 * time.Minute: "2h3m",
	}
	for in, want := range cases {
		if got := shortDur(in); got != want {
			t.Errorf("shortDur(%v) = %q, want %q", in, got, want)
		}
	}
}
