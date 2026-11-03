package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// fallbackClient implements hermes.ChatClient: while the coding worker is
// deep in a job it answers from the small fallback engine (the always-on
// iGPU helper model, or a CPU engine if pointed at one), and from the
// primary brain otherwise. The brain is saturated during a worker round —
// long context, continuous decode — and Discord replies queue behind it.
//
// The "worker busy" signal is the job store: agent-run marks a job's
// state.json status "running" for the whole round. Results are cached
// briefly so message bursts do not rescan the store per call.
type fallbackClient struct {
	primary *models.Client
	fb      *models.Client // client for the fallback engine (lazy)
	fbURL   string
	fbModel string
	jobsDir string

	mu        sync.Mutex
	busyAt    time.Time
	busyVal   bool
	upAt      time.Time
	upVal     bool
	lastRoute string
}

const (
	fallbackBusyTTL = 5 * time.Second
	fallbackUpTTL   = 15 * time.Second
)

// NewFallbackClient wraps primary with worker-busy routing. The fallback
// engine comes from FALLBACK_ENGINE_URL / FALLBACK_MODEL (empty fbURL makes
// the wrapper a pass-through); JOBS_DIR overrides the job store location.
func NewFallbackClient(primary *models.Client) *fallbackClient {
	fbURL := os.Getenv("FALLBACK_ENGINE_URL")
	fbModel := os.Getenv("FALLBACK_MODEL")
	fc := &fallbackClient{
		primary: primary,
		fbURL:   fbURL,
		fbModel: fbModel,
		jobsDir: os.Getenv("JOBS_DIR"),
	}
	if fbURL != "" {
		fc.fb = models.NewClientWithBase(fbURL)
	}
	return fc
}

// Chat routes the call per workerBusy()/fallbackUp() and remembers the
// route choice for logging.
func (f *fallbackClient) Chat(model string, messages []models.ChatMessage) (string, error) {
	if f.fb != nil && f.fbURL != "" && f.fbModel != "" {
		if busy, err := f.workerBusy(); err == nil && busy && f.fallbackUp() {
			f.setRoute("fallback")
			return f.fb.Chat(f.fbModel, messages)
		}
	}
	f.setRoute("primary")
	return f.primary.Chat(model, messages)
}

// workerBusy reports whether any job in the store has status "running".
// The scan result is cached for fallbackBusyTTL. A missing store is not an
// error (treated as idle): routing is a latency optimization, not a
// correctness dependency.
func (f *fallbackClient) workerBusy() (bool, error) {
	dir := f.jobsDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false, err
		}
		dir = filepath.Join(home, ".local", "share", "jarvis", "jobs")
	}
	f.mu.Lock()
	if time.Since(f.busyAt) < fallbackBusyTTL {
		v := f.busyVal
		f.mu.Unlock()
		return v, nil
	}
	f.mu.Unlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	busy := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "state.json"))
		if err != nil {
			continue
		}
		var st struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(b, &st) == nil && st.Status == "running" {
			busy = true
			break
		}
	}
	f.mu.Lock()
	f.busyAt = time.Now()
	f.busyVal = busy
	f.mu.Unlock()
	return busy, nil
}

// fallbackUp probes the fallback engine's llama.cpp /health, cached for
// fallbackUpTTL so a dead engine does not add a full timeout to every call.
func (f *fallbackClient) fallbackUp() bool {
	f.mu.Lock()
	if time.Since(f.upAt) < fallbackUpTTL {
		v := f.upVal
		f.mu.Unlock()
		return v
	}
	f.mu.Unlock()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(f.fbURL + "/health")
	up := false
	if err == nil {
		up = resp.StatusCode == http.StatusOK
		resp.Body.Close()
	}
	f.mu.Lock()
	f.upAt = time.Now()
	f.upVal = up
	f.mu.Unlock()
	return up
}

// setRoute logs a route change once (a worker round lasts minutes; logging
// every call would spam the journal).
func (f *fallbackClient) setRoute(route string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastRoute != route {
		log.Printf("jarvis-chat: LLM route -> %s (fbURL=%s fbModel=%s)", route, f.fbURL, f.fbModel)
		f.lastRoute = route
	}
}
