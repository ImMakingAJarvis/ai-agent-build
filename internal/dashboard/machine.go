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

// machineVRAM is the 4090's video memory and power from the nvidia
// exporter.
type machineVRAM struct {
	UsedMB  int64   `json:"used_mb"`
	TotalMB int64   `json:"total_mb"`
	PowerW  float64 `json:"power_w"`
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

// machineLease is one active ledger lease (resourced GET /state): who
// holds which device's memory, and since when.
type machineLease struct {
	Holder     string `json:"holder"`
	Capability string `json:"capability,omitempty"`
	VRAMMB     int64  `json:"vram_mb"`
	Since      string `json:"since"` // time of day, container local
}

// machineDeviceLeases is the lease card's one device row (EVB-30: the
// spec asks for leases per device).
type machineDeviceLeases struct {
	Device string         `json:"device"`
	Leases []machineLease `json:"leases"`
}

// machineWindow is the broker's view of the swap window (resourced
// GET /state "window" — richer than the state file the panel already
// reads: the last decision's reason and the tick's depth snapshot).
type machineWindow struct {
	Open   bool     `json:"open"`
	For    []string `json:"for,omitempty"`
	Age    string   `json:"age,omitempty"`    // "45s" / "12m" while open
	Reason string   `json:"reason,omitempty"` // last decision's reason
	Up     bool     `json:"up"`               // /state was reachable
}

// machineCapDepth is one capability's queue depth from the broker's
// snapshot: ready count plus the in-flight (unacked) deliveries.
type machineCapDepth struct {
	Cap   string `json:"cap"`
	Depth int    `json:"depth"`
}

// machineWaiting is a brain job in the waiting status: what it waits on
// (the continuation's correlation ids), how long, and the note its
// round left.
type machineWaiting struct {
	ID            string   `json:"id"`
	Title         string   `json:"title,omitempty"`
	WaitOn        []string `json:"wait_on"`
	WaitNote      string   `json:"wait_note,omitempty"`
	WaitAge       string   `json:"wait_age"`
	WaitRemaining string   `json:"wait_remaining,omitempty"`
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
	// The event-driven-broker half of the Machine view (EVB-30): the
	// window and queue state resourced owns, not just the swap file.
	Window    *machineWindow        `json:"window,omitempty"`
	Leases    []machineDeviceLeases `json:"leases,omitempty"`
	CapDepths []machineCapDepth     `json:"cap_depths,omitempty"`
	Waiting   []machineWaiting      `json:"waiting,omitempty"`
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

// parseVRAMMetrics extracts the first GPU's used/total VRAM and power
// draw from the nvidia exporter's Prometheus text format; nil when the
// metrics are absent.
func parseVRAMMetrics(body string) *machineVRAM {
	var used, total int64
	var power float64
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "nvidia_gpu_mem_used_mb "):
			used = metricValue(line)
		case strings.HasPrefix(line, "nvidia_gpu_mem_total_mb "):
			total = metricValue(line)
		case strings.HasPrefix(line, "nvidia_gpu_power_w "):
			power = metricValueF(line)
		}
	}
	if total <= 0 {
		return nil
	}
	return &machineVRAM{UsedMB: used, TotalMB: total, PowerW: power}
}

// metricValue is the last field of a Prometheus sample line.
func metricValue(line string) int64 {
	v, _ := strconv.ParseInt(metricValueFStr(line), 10, 64)
	return v
}

// metricValueF is the last field of a Prometheus sample line as a float
// (the power sample is not integral).
func metricValueF(line string) float64 {
	v, _ := strconv.ParseFloat(metricValueFStr(line), 64)
	return v
}

func metricValueFStr(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "0"
	}
	return fields[len(fields)-1]
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

// resourcedProbe is what the Machine view needs from the broker's
// GET /state (EVB-30): the leases grouped per device, the window state
// and the per-capability queue-depth snapshot.
type resourcedProbe struct {
	Window    machineWindow
	Leases    []machineDeviceLeases
	CapDepths []machineCapDepth
}

// probeResourced reads the broker's /state (EVB-30). Nil when resourced
// is down — the panel degrades to the swap-file view it had before.
func (s *Server) probeResourced() *resourcedProbe {
	body, err := s.getBody(s.resourcedURL + "/state")
	if err != nil {
		return nil
	}
	var out struct {
		Leases []struct {
			Holder     string    `json:"holder"`
			Capability string    `json:"capability"`
			Device     string    `json:"device"`
			VRAMMB     int64     `json:"vram_mb"`
			Since      time.Time `json:"since"`
		} `json:"leases"`
		Window struct {
			Open      bool          `json:"open"`
			For       []string      `json:"for"`
			OpenedAt  time.Time     `json:"opened_at"`
			Reason    string        `json:"reason"`
			CapDepths map[string]int `json:"cap_depths"` // the tick's depth snapshot (EVB-30)
		} `json:"window"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil
	}
	p := &resourcedProbe{}
	if out.Window.Open {
		p.Window = machineWindow{Open: true, For: out.Window.For, Reason: out.Window.Reason, Up: true}
		if !out.Window.OpenedAt.IsZero() {
			p.Window.Age = shortDur(time.Since(out.Window.OpenedAt))
		}
	} else {
		p.Window = machineWindow{Reason: out.Window.Reason, Up: true}
	}
	byDev := map[string]*machineDeviceLeases{}
	var order []string
	for _, l := range out.Leases {
		d, ok := byDev[l.Device]
		if !ok {
			d = &machineDeviceLeases{Device: l.Device}
			byDev[l.Device] = d
			order = append(order, l.Device)
		}
		d.Leases = append(d.Leases, machineLease{
			Holder: l.Holder, Capability: l.Capability, VRAMMB: l.VRAMMB,
			Since: l.Since.Local().Format("15:04"),
		})
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	for _, d := range order {
		p.Leases = append(p.Leases, *byDev[d])
	}
	// The depth snapshot rides the window field (the controller's tick
	// writes both together); absent = no tick has run yet.
	var depths []machineCapDepth
	for cap, n := range out.Window.CapDepths {
		depths = append(depths, machineCapDepth{Cap: cap, Depth: n})
	}
	sort.Slice(depths, func(i, j int) bool { return depths[i].Cap < depths[j].Cap })
	p.CapDepths = depths
	return p
}

// shortDur renders a duration as the panel's short form: "45s", "12m",
// "2h3m" (the same buckets as the waiting rows).
func shortDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// waitingBrainJobs picks the waiting (yielded) local jobs out of the
// cached job list — the ones a window serves and the panel must show
// waiting, with what each waits on.
func waitingBrainJobs(jobs []agentjob.Job, now time.Time) []machineWaiting {
	var out []machineWaiting
	for _, j := range jobs {
		if j.Status != agentjob.Waiting || (j.Driver != "" && j.Driver != "local") {
			continue
		}
		w := machineWaiting{
			ID: j.ID, Title: j.Title, WaitOn: j.WaitOn, WaitNote: j.WaitNote,
			WaitAge:       agentjob.WaitAge(j, now),
			WaitRemaining: agentjob.WaitRemaining(j, now),
		}
		if len(w.WaitOn) == 0 {
			w.WaitOn = []string{"—"}
		}
		out = append(out, w)
	}
	return out
}

// gatherMachine collects the machine panel. Every probe runs in parallel
// with its own timeout, and a dead endpoint degrades to a "down"/"—" card
// rather than failing the section.
func (s *Server) gatherMachine() MachineState {
	var st MachineState
	var brainUp, igpuUp, artUp bool
	var brainModel, igpuModel, artModel string
	var rsProbe *resourcedProbe
	var wg sync.WaitGroup
	wg.Add(6)
	go func() { defer wg.Done(); brainUp, brainModel = s.probeEngine(s.engineURL) }()
	go func() { defer wg.Done(); igpuUp, igpuModel = s.probeEngine(s.igpuURL) }()
	go func() { defer wg.Done(); artUp, artModel = s.probeArt() }()
	go func() { defer wg.Done(); st.VRAM = s.probeVRAM() }()
	go func() { defer wg.Done(); st.Measured = s.probeMeasured() }()
	go func() { defer wg.Done(); rsProbe = s.probeResourced() }()
	wg.Wait()

	// The window is open when jarvis-swap's state file exists; art being up
	// with no state file is the closing edge (file removed, art still
	// loading out).
	windowOpen, windowDetail := swapWindowState(s.windowPath)
	if !windowOpen && artUp {
		windowOpen = true
	}
	// The broker's view is the controller's — when it is reachable it
	// wins (it also carries the reason and the depth snapshot).
	if rsProbe != nil && rsProbe.Window.Up {
		st.Window = &rsProbe.Window
		st.Leases = rsProbe.Leases
		st.CapDepths = rsProbe.CapDepths
		windowOpen = windowOpen || rsProbe.Window.Open
		if rsProbe.Window.Open {
			d := "for " + strings.Join(rsProbe.Window.For, ", ")
			if rsProbe.Window.Reason != "" {
				d += " — " + rsProbe.Window.Reason
			}
			windowDetail = d
		}
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

	jobs := s.getCachedJobData().agentJobs
	st.Hold = holdCounts(jobs)
	st.Waiting = waitingBrainJobs(jobs, time.Now())
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
