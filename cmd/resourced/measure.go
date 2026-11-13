package main

// VRAM footprint measurements (plan §3.6, EVB-28). The capability
// catalog declares a budget per placement; on its first run each
// capability worker measures the model's actual nvidia-smi delta and
// reports it here (POST /measure, from the capworker SDK). resourced
//
//	- keeps the measurements in a runtime-state overlay — a JSON file
//	  next to the swap-window state (MEASUREMENTS_FILE, default
//	  ~/.local/state/jarvis/cap-measurements.json). Deliberately NOT a
//	  rewrite of config/capabilities.yaml: that file is tracked in the
//	  repo and bind-mounted read-only into the worker containers, and a
//	  runtime measurement is machine state, not a catalog entry;
//	- budgets future lease requests by max(declared, measured) — a
//	  worker that measures bigger than it declares is charged the truth
//	  (handleLeasePost);
//	- alerts when measured > declared: a worker.<cap@device>.measured
//	  event with over=true (the dashboard shows the overlay at
//	  GET /measured).
//
// The overlay survives resourced restarts (the ledger itself is
// in-memory) and is deletable per entry (DELETE /measured/<cap@device>)
// so a stale or hand-tested measurement can be cleared.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// measurement is one capability's recorded footprint.
type measurement struct {
	Capability string    `json:"capability"`
	Device     string    `json:"device"`
	Holder     string    `json:"holder,omitempty"`
	DeclaredMB int64     `json:"declared_mb"`
	MeasuredMB int64     `json:"measured_mb"`
	Over       bool      `json:"over"` // measured > declared: the alert flag
	UpdatedAt  time.Time `json:"updated_at"`
}

// measKey is the overlay's map key: capability@device.
func measKey(cap, device string) string {
	return cap + "@" + device
}

// measurementStore is the overlay: capability@device → measurement,
// persisted atomically (tmp + rename) on every record.
type measurementStore struct {
	mu   sync.Mutex
	path string
	log  *slog.Logger
	m    map[string]measurement
}

func newMeasurementStore(path string, log *slog.Logger) *measurementStore {
	s := &measurementStore{path: path, log: log, m: make(map[string]measurement)}
	b, err := os.ReadFile(path)
	if err == nil {
		if jerr := json.Unmarshal(b, &s.m); jerr != nil {
			log.Error("measured VRAM overlay unreadable — starting empty", "path", path, "err", jerr)
			s.m = make(map[string]measurement)
		}
	}
	if len(s.m) > 0 {
		log.Info("measured VRAM overlay loaded", "path", path, "entries", len(s.m))
	}
	return s
}

// record stores one measurement (recomputing over from the numbers —
// the flag is the ledger's, not the worker's) and persists it. It
// returns the stored record (the recomputed flag and the timestamp).
func (s *measurementStore) record(m measurement) (measurement, error) {
	m.Over = m.MeasuredMB > m.DeclaredMB
	m.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[measKey(m.Capability, m.Device)] = m
	return m, s.saveLocked()
}

// budget is the VRAM a future lease for this capability is charged:
// the declared value, bumped to the measured one when the worker
// measures bigger than it declares. Unknown entries cost the declared
// value.
func (s *measurementStore) budget(cap, device string, declared int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.m[measKey(cap, device)]; ok && m.MeasuredMB > declared {
		return m.MeasuredMB
	}
	return declared
}

// all returns a copy of the overlay in key order (GET /measured).
func (s *measurementStore) all() map[string]measurement {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]measurement, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out
}

// forget deletes one entry (DELETE /measured/<cap@device>); false when
// there is none.
func (s *measurementStore) forget(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; !ok {
		return false
	}
	delete(s.m, key)
	return s.saveLocked() == nil
}

// saveLocked writes the overlay atomically (tmp + rename) so a crash
// cannot leave a torn file.
func (s *measurementStore) saveLocked() error {
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ---------------- the HTTP handlers ----------------

// handleMeasurePost is POST /measure — one worker's footprint report
// (capworker, EVB-28). 201 on store; the alert (when measured >
// declared) is the worker.<cap@device>.measured event with over=true.
func (s *service) handleMeasurePost(w http.ResponseWriter, r *http.Request) {
	var m measurement
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, fmt.Sprintf("bad measurement: %v", err), http.StatusBadRequest)
		return
	}
	if m.Capability == "" || m.Device == "" || m.DeclaredMB < 0 || m.MeasuredMB < 0 {
		http.Error(w, "capability, device, declared_mb and measured_mb are required (non-negative)", http.StatusBadRequest)
		return
	}
	m, err := s.measured.record(m)
	if err != nil {
		s.log.Error("measured VRAM overlay write", "err", err)
		http.Error(w, "could not persist the measurement", http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"capability":  m.Capability,
		"device":      m.Device,
		"declared_mb": m.DeclaredMB,
		"measured_mb": m.MeasuredMB,
		"over":        m.Over,
	}
	if m.Holder != "" {
		data["holder"] = m.Holder
	}
	if m.Over {
		// The alert: the catalog under-declared this placement.
		s.log.Warn("measured VRAM exceeds declared", "capability", m.Capability,
			"device", m.Device, "declared_mb", m.DeclaredMB, "measured_mb", m.MeasuredMB)
	}
	s.emit(r.Context(), queue.WorkerEvent(measKey(m.Capability, m.Device), "measured", data))
	writeJSON(w, http.StatusCreated, map[string]any{"status": "recorded", "measured": m})
}

// handleMeasuredGet is GET /measured — the overlay (the dashboard's
// machine panel shows it: declared → measured per capability).
func (s *service) handleMeasuredGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"measurements": s.measured.all()})
}

// handleMeasuredDelete is DELETE /measured/<cap@device> — clears one
// entry (a stale or hand-tested measurement).
func (s *service) handleMeasuredDelete(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/measured/")
	if key == "" || strings.Contains(key, "/") {
		http.Error(w, "want /measured/<capability@device>", http.StatusBadRequest)
		return
	}
	if !s.measured.forget(key) {
		http.Error(w, fmt.Sprintf("no measurement for %q", key), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "key": key})
}
