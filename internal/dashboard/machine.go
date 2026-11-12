// The machine panel: the state of the 4090 and what is waiting on it.
//
// The queue section shows the work, but not the machine it runs on. This
// panel answers the four things the queue cannot: which model is loaded on
// the GPU (brain, iGPU helper, art), whether a brain-swap window is in
// flight ("any changes happening"), how much VRAM is free ("what's in ram"),
// and what is on hold for the GPU ("what's on hold" — the photo batch plus
// the rest of the ready queue).
package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"sort"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photo"
)

// machineEngine is one loaded-model card: what is running where.
type machineEngine struct {
	Name  string `json:"name"`
	State string `json:"state"` // up | down | idle | swapped
	Model string `json:"model,omitempty"`
}

// machineVRAM is the 4090's video memory from the nvidia exporter.
type machineVRAM struct {
	UsedMB  int64 `json:"used_mb"`
	TotalMB int64 `json:"total_mb"`
}

// machineMeasured is one entry of resourced's measured-VRAM overlay
// (EVB-28): what the capability declared in the catalog and what its
// first run actually took off the GPU. Over = the alert case (the
// catalog under-declared).
type machineMeasured struct {
	Cap        string `json:"cap"` // capability@device
	DeclaredMB int64  `json:"declared_mb"`
	MeasuredMB int64  `json:"measured_mb"`
	Over       bool   `json:"over"`
}

// FreeMB is what a photo job could take from the GPU.
func (v machineVRAM) FreeMB() int64 { return v.TotalMB - v.UsedMB }

// machineHold is the work waiting on the machine.
type machineHold struct {
	Photo   int `json:"photo"`   // photo-tagged ready jobs: the swap window's batch
	Ready   int `json:"ready"`   // the rest of the ready queue
	Running int `json:"running"` // claimed/running rounds
	Failed  int `json:"failed"`  // failed rounds still in the store
}

// MachineState is the body of the machine section (GET /html/machine).
type MachineState struct {
	Swap       string            `json:"swap"`        // "brain running" | "swap window open" | "brain down"
	SwapTone   string            `json:"swap_tone"`   // ok | warn | bad
	SwapDetail string            `json:"swap_detail,omitempty"`
	Engines    []machineEngine   `json:"engines"`
	VRAM       *machineVRAM      `json:"vram,omitempty"`
	Measured   []machineMeasured `json:"measured,omitempty"` // resourced's VRAM overlay (EVB-28)
	Hold       machineHold       `json:"hold"`
	LastSwap   string            `json:"last_swap,omitempty"`
}

// probeTimeout bounds one machine probe: the section refreshes every 5 s, so
// a dead endpoint must not hold the rest of the panel up.
const probeTimeout = 2 * time.Second

// DivGB renders a megabyte count as gigabytes with one decimal (the VRAM
// card).
func DivGB(mb int64) string {
	return strconv.FormatFloat(float64(mb)/1024, 'f', 1, 64)
}

// swapState derives the panel's headline from the raw probes.
func swapState(brainUp, windowOpen bool, windowDetail, artModel string) (label, tone, detail string) {
	switch {
	case brainUp:
		return "brain running", "ok", ""
	case windowOpen:
		if windowDetail == "" {
			windowDetail = "brain paused for GPU work"
		}
		if artModel != "" {
			windowDetail += " — " + artModel + " on the 4090"
		}
		return "swap window open", "warn", windowDetail
	default:
		return "brain down", "bad",
			"no swap window open — the watchdog re-arms the brain at the deadline"
	}
}

// engineState is a card's state: the brain is "swapped" (expected) while a
// window is open, "down" (bad) otherwise; a plain up/down for the rest.
func engineState(up, windowOpen bool) string {
	if up {
		return "up"
	}
	if windowOpen {
		return "swapped"
	}
	return "down"
}

// modelDisplayName turns a /v1/models name into a card label: llama.cpp
// reports the GGUF path, and the card wants the model's name.
func modelDisplayName(name string) string {
	return strings.TrimSuffix(filepath.Base(name), ".gguf")
}

// parseVRAMMetrics extracts the first GPU's used/total VRAM from the nvidia
// exporter's Prometheus text format; nil when the metrics are absent.
func parseVRAMMetrics(body string) *machineVRAM {
	var used, total int64
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "nvidia_gpu_mem_used_mb "):
			used = metricValue(line)
		case strings.HasPrefix(line, "nvidia_gpu_mem_total_mb "):
			total = metricValue(line)
		}
	}
	if total <= 0 {
		return nil
	}
	return &machineVRAM{UsedMB: used, TotalMB: total}
}

// metricValue is the last field of a Prometheus sample line.
func metricValue(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseInt(fields[len(fields)-1], 10, 64)
	return v
}

// swapEventDetail pulls the human detail out of a swap event line: the
// reason= field when present (it may contain nested parens), else "".
func swapEventDetail(rest string) string {
	i := strings.Index(rest, "reason=")
	if i < 0 {
		return ""
	}
	d := rest[i+len("reason="):]
	if j := strings.LastIndex(d, ")"); j >= 0 {
		d = d[:j]
	}
	d = strings.TrimSuffix(d, "===")
	d = strings.TrimRight(d, " ")
	if d == "none" {
		return ""
	}
	return d
}

// parseSwapLog returns the last swap event in the jarvis-swap log, formatted
// as "YYYY-MM-DD HH:MM IN|OUT[: reason]"; "" when the log has none.
func parseSwapLog(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	const marker = "[jarvis-swap] === swap "
	for i := len(lines) - 1; i >= 0; i-- {
		idx := strings.Index(lines[i], marker)
		if idx < 0 {
			continue
		}
		rest := lines[i][idx+len(marker):]
		dir := strings.Fields(rest)
		// Event lines carry a "(...)" detail group; "=== swap IN complete ==="
		// does not and must be skipped.
		if len(dir) == 0 || (dir[0] != "IN" && dir[0] != "OUT") || !strings.Contains(rest, " (") {
			continue
		}
		when := ""
		if len(lines[i]) >= 16 {
			when = lines[i][:16] // "YYYY-MM-DD HH:MM"
		}
		s := dir[0]
		if when != "" {
			s = when + " " + s
		}
		if d := swapEventDetail(rest); d != "" {
			s += ": " + d
		}
		return s
	}
	return ""
}

// swapWindowState reads the state file jarvis-swap writes next to
// swap.log when a window is open: {"for":[...],"reason":"...","opened_at":
// "RFC3339"} (EVB-27 — the window is driven by resourced, not the
// queue watcher's old $CONF/photo-window marker). It reports whether the
// window is open and a one-line detail (when it opened, and the
// capabilities it serves). A file that exists but does not parse still
// counts as open: it was written early by `out`, and a truncated read
// should read "window", not "brain down".
func swapWindowState(path string) (open bool, detail string) {
	b, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return false, ""
	}
	var st struct {
		For      []string `json:"for"`
		OpenedAt string   `json:"opened_at"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return true, ""
	}
	if t, err := time.Parse(time.RFC3339, st.OpenedAt); err == nil {
		detail = t.Format("2006-01-02 15:04")
	}
	if len(st.For) > 0 {
		if detail != "" {
			detail += " — "
		}
		detail += "for " + strings.Join(st.For, ", ")
	}
	return true, detail
}

// holdCounts sums the ready/running/failed jobs by kind; photo-tagged ready
// jobs count separately (they are the swap window's batch, not the band
// queue).
func holdCounts(jobs []agentjob.Job) machineHold {
	var h machineHold
	for _, j := range jobs {
		switch j.Status {
		case agentjob.Ready:
			if j.HasTag(photo.Tag) {
				h.Photo++
			} else {
				h.Ready++
			}
		case agentjob.Claimed, agentjob.Running:
			h.Running++
		case agentjob.Failed:
			h.Failed++
		}
	}
	return h
}

// getBody fetches url and returns the body, or an error. Bodies are capped:
// these are small JSON/metrics payloads, never transcripts.
func (s *Server) getBody(url string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	res, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", url, res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// probeEngine asks one engine's /v1/models for its loaded model.
func (s *Server) probeEngine(base string) (up bool, model string) {
	body, err := s.getBody(base + "/v1/models")
	if err != nil {
		return false, ""
	}
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Models) == 0 {
		return false, ""
	}
	return true, modelDisplayName(out.Models[0].Name)
}

// probeArt asks the art service's /health; it is only up in a photo window.
func (s *Server) probeArt() (up bool, model string) {
	body, err := s.getBody(s.artURL + "/health")
	if err != nil {
		return false, ""
	}
	var h struct {
		Status string `json:"status"`
		Model  string `json:"model"`
		Loaded bool   `json:"loaded"`
	}
	if err := json.Unmarshal([]byte(body), &h); err != nil || !h.Loaded {
		return false, ""
	}
	return true, h.Model
}

// probeVRAM reads the 4090's memory from the nvidia exporter.
func (s *Server) probeVRAM() *machineVRAM {
	body, err := s.getBody(s.nvidiaURL + "/metrics")
	if err != nil {
		return nil
	}
	return parseVRAMMetrics(body)
}

// probeMeasured reads resourced's measured-VRAM overlay (EVB-28): the
// capabilities whose first run has measured their actual GPU footprint,
// in stable (key) order. Nil when resourced is down or has none yet.
func (s *Server) probeMeasured() []machineMeasured {
	body, err := s.getBody(s.resourcedURL + "/measured")
	if err != nil {
		return nil
	}
	var out struct {
		Measurements map[string]machineMeasured `json:"measurements"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Measurements) == 0 {
		return nil
	}
	rows := make([]machineMeasured, 0, len(out.Measurements))
	for key, m := range out.Measurements {
		m.Cap = key
		rows = append(rows, m)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Cap < rows[j].Cap })
	return rows
}

// gatherMachine collects the machine panel. Every probe runs in parallel
// with its own timeout, and a dead endpoint degrades to a "down"/"—" card
// rather than failing the section.
func (s *Server) gatherMachine() MachineState {
	var st MachineState
	var brainUp, igpuUp, artUp bool
	var brainModel, igpuModel, artModel string
	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); brainUp, brainModel = s.probeEngine(s.engineURL) }()
	go func() { defer wg.Done(); igpuUp, igpuModel = s.probeEngine(s.igpuURL) }()
	go func() { defer wg.Done(); artUp, artModel = s.probeArt() }()
	go func() { defer wg.Done(); st.VRAM = s.probeVRAM() }()
	go func() { defer wg.Done(); st.Measured = s.probeMeasured() }()
	wg.Wait()

	// The window is open when jarvis-swap's state file exists; art being up
	// with no state file is the closing edge (file removed, art still
	// loading out).
	windowOpen, windowDetail := swapWindowState(s.windowPath)
	if !windowOpen && artUp {
		windowOpen = true
	}

	label, tone, detail := swapState(brainUp, windowOpen, windowDetail, artModel)
	st.Swap, st.SwapTone, st.SwapDetail = label, tone, detail

	st.Engines = []machineEngine{
		{Name: "Brain (4090)", State: engineState(brainUp, windowOpen), Model: brainModel},
		{Name: "iGPU helper", State: engineState(igpuUp, false), Model: igpuModel},
	}
	if artUp {
		st.Engines = append(st.Engines, machineEngine{Name: "Art (4090)", State: "up", Model: artModel})
	} else {
		st.Engines = append(st.Engines, machineEngine{Name: "Art (4090)", State: "idle"})
	}

	st.Hold = holdCounts(s.getCachedJobData().agentJobs)
	st.LastSwap = parseSwapLog(s.swapLog)
	return st
}

// handleMachineHTML renders the machine section (HTMX, every 5 s).
func (s *Server) handleMachineHTML(w http.ResponseWriter, r *http.Request) {
	data := s.gatherMachine()
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "machine-section", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}
