// Package power measures the electricity a job uses: it samples the GPU's
// power draw (nvidia-smi) while the job runs, accumulates watt-hours, and
// converts them to kWh and dollars at a configurable $/kWh rate. The
// agent runner starts one Sampler per job; the accumulated energy is
// recorded on the job (state.json) and in the job history (jobs.jsonl).
//
// Environment:
//
//	JARVIS_POWER_RATE        electricity price in $/kWh (e.g. 0.15).
//	                         Empty or 0 = track kWh but cost stays $0.
//	JARVIS_POWER_SYSTEM_W    flat non-GPU system overhead in watts (CPU,
//	                         RAM, disks, PSU losses) added for the whole
//	                         run. Default 0 — set it if you want whole-
//	                         machine cost, not just GPU cost.
//	JARVIS_POWER_SAMPLE_SECS sampling interval in seconds (float ok).
//	                         Default 10.
//	JARVIS_POWER_GPU_CMD     nvidia-smi path (tests). Default "nvidia-smi".
//
// Accuracy: power is sampled, not metered. Each sample integrates watts
// over the time elapsed since the previous sample, so the error is
// bounded by one sampling interval.
package power

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PersistFile is the power.json name inside a job directory.
const PersistFile = "power.json"

// Rate returns the configured electricity price in $/kWh (0 if unset).
func Rate() float64 { return envFloat("JARVIS_POWER_RATE", 0) }

// SystemWatts returns the flat non-GPU system overhead in watts (0 if unset).
func SystemWatts() float64 { return envFloat("JARVIS_POWER_SYSTEM_W", 0) }

// Interval returns the sampling interval (default 10s).
func Interval() time.Duration {
	return time.Duration(envFloat("JARVIS_POWER_SAMPLE_SECS", 10)*1000) * time.Millisecond
}

func envFloat(key string, def float64) float64 {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return def
	}
	return v
}

// PowerDraw reads the current GPU power draw in watts (first GPU).
func PowerDraw() (float64, error) {
	cmdName := "nvidia-smi"
	if c := os.Getenv("JARVIS_POWER_GPU_CMD"); c != "" {
		cmdName = c
	}
	out, err := exec.Command(cmdName,
		"--query-gpu=power.draw",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, err
	}
	// nvidia-smi CSV: "364.53" (one field per GPU) — take the first.
	s := strings.TrimSpace(strings.ReplaceAll(string(out), ",", " "))
	if i := strings.IndexByte(s, ' '); i >= 0 {
		s = s[:i]
	}
	return strconv.ParseFloat(s, 64)
}

// Kwh converts watt-hours to kilowatt-hours.
func Kwh(wh float64) float64 { return wh / 1000 }

// Cost converts energy in watt-hours to dollars at the given $/kWh rate.
func Cost(wh, rate float64) float64 { return Kwh(wh) * rate }

// snapshot is the persisted accumulation (power.json in the job dir).
type snapshot struct {
	Wh        float64 `json:"wh"`
	Samples   int     `json:"samples"`
	StartedAt string  `json:"started_at"`
	UpdatedAt string  `json:"updated_at"`
}

// Sampler accumulates watt-hours while a job runs. It persists the
// running total to <jobDir>/power.json on every sample, so a crash or
// reboot loses at most one sampling interval, and a job re-run (after a
// pause or a failure) accumulates on top of the earlier attempts — a
// job's cost is its total energy across all attempts.
type Sampler struct {
	dir      string
	interval time.Duration

	mu      sync.Mutex
	wh      float64
	samples int
	started time.Time
	last    time.Time
	stop    chan struct{}
	done    chan struct{}
	stopped bool
}

// NewSampler creates a sampler that persists into dir with the
// configured interval.
func NewSampler(dir string) *Sampler {
	return &Sampler{
		dir:      dir,
		interval: Interval(),
		started:  time.Now().UTC(),
	}
}

// Start begins sampling in a background goroutine. It loads any
// previously persisted total first, so restarts accumulate.
func (s *Sampler) Start() {
	s.mu.Lock()
	if prev, err := s.load(); err == nil {
		s.wh = prev.Wh
		s.samples = prev.Samples
	}
	s.last = time.Now()
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.mu.Unlock()
	go s.loop()
}

func (s *Sampler) loop() {
	defer close(s.done)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.sample()
		}
	}
}

// sample reads the GPU once and integrates watts over the time elapsed
// since the previous sample. A failed read is skipped; the next sample
// covers the whole elapsed span at the new rate.
func (s *Sampler) sample() {
	now := time.Now()
	w, err := PowerDraw()
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// watts × hours = watt-hours (seconds → hours).
	s.wh += (w + SystemWatts()) * now.Sub(s.last).Seconds() / 3600
	s.samples++
	s.last = now
	s.persistLocked()
}

// Stop halts sampling and returns the accumulated watt-hours. It is safe
// to call twice; a sampler that never started returns 0.
func (s *Sampler) Stop() float64 {
	s.mu.Lock()
	if s.stop == nil || s.stopped {
		s.mu.Unlock()
		return s.wh
	}
	s.stopped = true
	close(s.stop)
	s.mu.Unlock()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistLocked()
	return s.wh
}

func (s *Sampler) persistLocked() {
	sn := snapshot{
		Wh:        s.wh,
		Samples:   s.samples,
		StartedAt: s.started.Format(time.RFC3339),
		UpdatedAt: s.last.Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(sn, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(s.dir, PersistFile), b, 0o644)
}

func (s *Sampler) load() (snapshot, error) {
	var sn snapshot
	b, err := os.ReadFile(filepath.Join(s.dir, PersistFile))
	if err != nil {
		return sn, err
	}
	return sn, json.Unmarshal(b, &sn)
}

// LoadPersisted returns the watt-hours persisted in dir's power.json
// (error if the file is missing). It is used when a round is recorded
// after a reboot, when no sampler ran in the current process.
func LoadPersisted(dir string) (float64, error) {
	var sn snapshot
	b, err := os.ReadFile(filepath.Join(dir, PersistFile))
	if err != nil {
		return 0, err
	}
	if err := json.Unmarshal(b, &sn); err != nil {
		return 0, err
	}
	return sn.Wh, nil
}
