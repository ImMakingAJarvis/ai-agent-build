package ha

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// --- TodayAPISpend ---

func writeJSONL(t *testing.T, path string, lines ...string) {
	t.Helper()
	var sb strings.Builder
	for i, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Fatalf("line %d is not JSON: %s", i, l)
		}
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rec(started, finished string, useLocal bool, est, actual float64) string {
	b, _ := json.Marshal(map[string]any{
		"id": "x", "type": "agent", "queue": "background",
		"started_at": started, "finished_at": finished, "status": "completed",
		"use_local":      useLocal,
		"estimated_cost": est, "actual_cost": actual,
	})
	return string(b)
}

func TestTodayAPISpendBasic(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	yest := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	path := filepath.Join(t.TempDir(), "jobs.jsonl")
	writeJSONL(t, path,
		rec(yest+"T23:00:00Z", yest+"T23:30:00Z", false, 5.0, 5.2), // yesterday: excluded
		rec(today+"T00:10:00Z", "", true, 0, 0),                    // today local: excluded
		rec(today+"T01:00:00Z", "", false, 1.5, 0),                 // today API, estimate only: +1.50
		rec(today+"T02:00:00Z", "", false, 2.0, 1.75),              // today API, actual wins: +1.75
		rec(today+"T03:00:00Z", "", false, 0.25, 0),                // today API small: +0.25
	)
	got, err := TodayAPISpend(path)
	if err != nil {
		t.Fatal(err)
	}
	want := 1.50 + 1.75 + 0.25
	if fmt.Sprintf("%.2f", got) != fmt.Sprintf("%.2f", want) {
		t.Errorf("spend = %v, want %v", got, want)
	}
}

func TestTodayAPISpendNoFile(t *testing.T) {
	if _, err := TodayAPISpend(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestTodayAPISpendOldCompletionDoesNotStopScan(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	yest := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	path := filepath.Join(t.TempDir(), "jobs.jsonl")

	// A: API job started today (+$1.50). Then filler (local, today) large
	// enough to push A into the chunk before the last one. Then B: a
	// completion line for a job started yesterday but finished today —
	// it sits in the newest chunk and must NOT stop the scan.
	a := rec(today+"T09:00:00Z", "", false, 1.5, 0)
	const chunk = 512 * 1024
	var filler []string
	size := len(a) + 1
	for size < chunk+100 { // last chunk must hold B but not A
		f := rec(today+"T09:30:00Z", "", true, 0, 0)
		filler = append(filler, f)
		size += len(f) + 1
	}
	b := rec(yest+"T23:50:00Z", today+"T00:10:00Z", false, 2.0, 2.1)
	lines := append([]string{a}, filler...)
	lines = append(lines, b)
	writeJSONL(t, path, lines...)

	got, err := TodayAPISpend(path)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%.2f", got) != "1.50" {
		t.Errorf("spend = %v, want 1.50 (old job's completion must not hide today's start)", got)
	}
}

func TestTodayAPISpendStopsAtOldRecords(t *testing.T) {
	today := time.Now().UTC().Format("2006-01-02")
	yest := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	path := filepath.Join(t.TempDir(), "jobs.jsonl")
	// Everything old except one today record: scan must sum only today's.
	lines := make([]string, 0, 50)
	for i := 0; i < 20; i++ {
		lines = append(lines, rec(yest+"T08:00:00Z", yest+"T09:00:00Z", false, 9.99, 9.99))
	}
	lines = append(lines, rec(today+"T10:00:00Z", "", false, 3.0, 0))
	for i := 0; i < 20; i++ {
		lines = append(lines, rec(yest+"T10:00:00Z", yest+"T11:00:00Z", false, 9.99, 9.99))
	}
	writeJSONL(t, path, lines...)
	got, err := TodayAPISpend(path)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%.2f", got) != "3.00" {
		t.Errorf("spend = %v, want 3.00", got)
	}
}

// --- GPUStats ---

func TestGPUStats(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-nvidia-smi")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"62, 370.59\"\n"), 0o755)
	t.Setenv("HA_GPU_CMD", fake)
	temp, power, err := GPUStats()
	if err != nil {
		t.Fatal(err)
	}
	if temp != 62 || fmt.Sprintf("%.2f", power) != "370.59" {
		t.Errorf("got temp=%v power=%v", temp, power)
	}
}

func TestGPUStatsMissing(t *testing.T) {
	t.Setenv("HA_GPU_CMD", filepath.Join(t.TempDir(), "no-such-cmd"))
	if _, _, err := GPUStats(); err == nil {
		t.Error("expected error when nvidia-smi is absent")
	}
}

// --- LoadedModel ---

func TestLoadedModelLlamaServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"models":[{"name":"/opt/jarvis-engine/models/jarvis-brain.gguf","model":"/opt/jarvis-engine/models/jarvis-brain.gguf"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	m, err := LoadedModel(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if m != "jarvis-brain.gguf" {
		t.Errorf("model = %q, want jarvis-brain.gguf", m)
	}
}

func TestLoadedModelOllama(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			http.Error(w, "nope", http.StatusMethodNotAllowed) // llama endpoint absent
		case "/api/ps":
			w.Write([]byte(`{"models":[{"name":"qwen3.8:27b"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	m, err := LoadedModel(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if m != "qwen3.8:27b" {
		t.Errorf("model = %q, want qwen3.8:27b", m)
	}
}

func TestLoadedModelNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	if _, err := LoadedModel(srv.URL); err == nil {
		t.Error("expected error when no endpoint answers")
	}
}

// --- CurrentJob ---

func seedJob(t *testing.T, store *agentjob.Store, id, title, status, claimedAt string) {
	t.Helper()
	if _, err := store.Ingest(agentjob.Spec{ID: id, Title: title}); err != nil {
		t.Fatal(err)
	}
	if status != agentjob.Ready {
		st := agentjob.State{Status: status, ClaimedAt: claimedAt, PID: 1}
		if err := store.SetStatus(id, st); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCurrentJobRunning(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	store, err := agentjob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	seedJob(t, store, "phase-old", "Old idle job", agentjob.Ready, "")
	seedJob(t, store, "phase-cur", "Current headline job", agentjob.Running, now)
	got, err := CurrentJob()
	if err != nil {
		t.Fatal(err)
	}
	if got != "Current headline job" {
		t.Errorf("current job = %q", got)
	}
}

func TestCurrentJobIdle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dir)
	store, err := agentjob.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedJob(t, store, "phase-done", "Done job", agentjob.Done, "")
	got, err := CurrentJob()
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("current job = %q, want empty when idle", got)
	}
}

// --- Entities / discovery ---

func TestDefaultEntities(t *testing.T) {
	ents := DefaultEntities("jarvis")
	if len(ents) != 9 {
		t.Fatalf("want 9 entities, got %d", len(ents))
	}
	seen := map[string]bool{}
	for _, e := range ents {
		if seen[e.UniqueID] {
			t.Errorf("duplicate unique id %s", e.UniqueID)
		}
		seen[e.UniqueID] = true
		if e.ValueTopic != "jarvis/sensor/"+e.UniqueID[len("jarvis_"):] {
			t.Errorf("%s: topic %q", e.UniqueID, e.ValueTopic)
		}
		if e.Name == "" || e.Icon == "" {
			t.Errorf("%s: missing name/icon", e.UniqueID)
		}
	}
}

func TestDiscoveryConfigShape(t *testing.T) {
	p := &Publisher{prefix: "jarvis", availability: "jarvis/availability", entities: DefaultEntities("jarvis")}
	for _, e := range p.entities {
		cfg := p.discoveryConfig(e)
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"name", "unique_id", "state_topic", "availability_topic", "value_template", "device"} {
			if _, ok := m[key]; !ok {
				t.Errorf("%s: config missing %q: %s", e.UniqueID, key, b)
			}
		}
		if m["unique_id"] != e.UniqueID || m["state_topic"] != e.ValueTopic {
			t.Errorf("%s: id/topic mismatch: %s", e.UniqueID, b)
		}
		dev, _ := m["device"].(map[string]any)
		if dev == nil || dev["name"] != "Jarvis Agent" {
			t.Errorf("%s: bad device block: %s", e.UniqueID, b)
		}
	}
}

func TestStateValue(t *testing.T) {
	s := State{
		Queues:        map[string]int{"voice": 3, "coding": 1},
		CurrentJob:    "do the thing",
		LoadedModel:   "jarvis-brain",
		APISpendToday: 12.5,
		GPUTemp:       62,
		GPUPower:      370.5,
		GPUOK:         true,
	}
	ents := DefaultEntities("jarvis")
	for _, e := range ents {
		v := stateValue(e, s)
		if v == "" || v == "unknown" {
			t.Errorf("%s: empty/unknown value with full state: %q", e.UniqueID, v)
		}
	}
	// Idle / unknown state must not publish blanks.
	empty := State{}
	for _, e := range ents {
		v := stateValue(e, empty)
		if v == "" {
			t.Errorf("%s: empty string for zero state", e.UniqueID)
		}
	}
}
