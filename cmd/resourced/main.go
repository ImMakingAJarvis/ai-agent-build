// resourced is the VRAM/RAM ledger and lease broker (plan task EVB-25,
// "the maximize-limited-hardware part" of the event-driven brain plan).
//
// It keeps the box's memory ledger in memory: the brain's standing GPU
// lease (measured live from nvidia-smi on every refresh, never
// hardcoded) plus the capability workers' granted leases and their wait
// queue. New requests are decided with the policy seed — grant if it
// fits in the free remainder (total − leases), queue if it does not but
// might later, deny with a reason if it never can. The full policy
// (conversation first, game watchdog, batch windows) lands with the
// window controller (EVB-27), which reuses this service's ledger.
//
// API (localhost HTTP, one port for API + metrics):
//
//	POST   /lease      {holder, capability, device, vram_mb, ram_mb, preemptible}
//	                      → 201 granted | 202 queued | 409 denied (reason)
//	DELETE /lease/<id>  → 200 released | 404 no such lease
//	GET    /state      → devices, active leases (incl. the brain's), queue
//	POST   /measure    {capability, device, declared_mb, measured_mb, holder}
//	                      → 201 recorded (worker.<cap@device>.measured event;
//	                       over=true when measured > declared — the alert)
//	GET    /measured   → the measured-VRAM overlay (cap@device → entry)
//	DELETE /measured/<capability@device> → 200 deleted | 404 none
//	GET    /healthz     → 200
//	GET    /metrics     → Prometheus
//
// Since EVB-27 it also runs the swap-window controller (plan §3.6,
// rules 3–4): it opens the brain swap window (jarvis-swap out --for
// <caps>) when queued GPU capability work exists and no local round is
// running, drains all of it, and closes the window (jarvis-swap in)
// when drained, at max_window, or when a ready brain job has waited
// past max_brain_wait. Every open/close publishes a window.<opened|
// closed> event; GET /state gains a "window" field (the broker's
// devices/leases/queued keys are unchanged).
//
// Since EVB-28 it co-schedules (plan §3.6, rule 5): a request is
// granted whenever it fits the free remainder (total − leases) — a
// small worker starts alongside a big one without a window; after a
// grant the queue is re-evaluated immediately. The brain's standing
// lease is measured total−free MINUS the co-tenants' declared GPU
// leases (their usage is in both, and double-counting would never
// leave room for the small ones). RAM requests are verified against
// the live free RAM, not just the pool. Workers report their actual
// VRAM footprint (POST /measure); the overlay budgets future leases
// and alerts when measured > declared.
//
// Since EVB-29 it runs the game/stream watchdog (plan §3.6 rule 2). The
// stream types (via the same internal/streamwatcher the host watchdogs
// use) split in two:
//
//   - A GAME (a Steam game running locally — detected by the game's
//     install path in argv — or Steam Remote Play) evicts the brain: the
//     ledger holds the GPU queue and revokes every capability worker's
//     GPU lease (granted AND queued, the brain's standing lease exempt),
//     and resourced stops the brain itself — jarvis-swap out --game
//     (the for:["game"] window: no photo services, just the VRAM freed,
//     re-armed by a 60s keepalive so long sessions outlive the 4h
//     maintenance deadline). On game end the brain comes back. resourced
//     owns this because it runs on the host; the worker container has no
//     docker socket and no view of the host's /proc.
//   - A Sunshine desktop stream takes NO GPU action (NVENC only, a few
//     hundred MB — it coexists with the brain and the workers); it only
//     logs, bumps the gauge, and publishes the event with game:false.
//
// The revoked workers run their task-26 drain; on game end the hold
// clears and the queued GPU leases grant again. CPU and iGPU leases are
// never touched (the rule is GPU-only). stream.started / stream.stopped
// events on the events exchange; the resourced_stream_active gauge;
// GET /state gains a "stream_active" field.
//
// Every grant/release/deny publishes a lease.<granted|released|denied>
// event on the events exchange. Configuration (env):
//
//	LISTEN_ADDR        API + metrics address. Default :9104.
//	REFRESH_INTERVAL   how often the live readings (and the brain's
//	                   standing lease) are refreshed. Default 30s.
//	WINDOW_INTERVAL    how often the window controller ticks.
//	                   Default 30s.
//	MAX_WINDOW         a window's hard cap. Default 2h (plan §7.2).
//	MAX_BRAIN_WAIT     close when a ready brain job has waited this
//	                   long. Default 20m (plan §7.2).
//	MIN_WINDOW         a drained window younger than this holds.
//	                   Default 2m (plan §7.2).
//	RABBITMQ_URL       the events broker (queue.NewConn's default when unset).
//	CAPABILITIES_FILE  the capability catalog (its needs_brain_out caps
//	                   are the queues the window drains). Default
//	                   config/capabilities.yaml.
//	SWAP_CMD           the swap script. Default jarvis-swap.
//	SWAP_STATE_FILE    the window state file jarvis-swap keeps. Default
//	                   ~/.local/state/jarvis/swap-window.json.
//	MEASUREMENTS_FILE  the measured-VRAM overlay (EVB-28). Default
//	                   ~/.local/state/jarvis/cap-measurements.json.
//	SUNSHINE_ENDPOINT  Sunshine API base for the stream watchdog (EVB-29).
//	                   Optional — the NVENC fallback needs no endpoint.
//	STEAM_ENDPOINT     Steam in-home streaming status page (EVB-29).
//	                   Optional, likewise.
//	STEAM_GAME_EXCLUDES  comma-separated command-line substrings that opt
//	                   a game out of the brain stop (EVB-29). Empty =
//	                   every Steam game counts.
//	BRAIN_URL            the brain engine the game keepalive probes (is
//	                   the brain back after a re-arm?). Default
//	                   http://127.0.0.1:11434.
//	SUNSHINE_STREAM_PORT the Sunshine video UDP port. Default 48010.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/catalog"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/fit"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/placement"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/resources"
)

// Publisher is the slice of queue.EventPublisher the service needs
// (tests substitute a recorder).
type Publisher interface {
	Publish(ctx context.Context, e queue.Event) error
}

// service is the broker: the ledger, the event publisher, the swap
// window controller (EVB-27) and the metrics. Handlers and the refresh
// loop all go through it.
type service struct {
	ledger   *resources.Ledger
	pub      Publisher
	readings fit.Source
	detect   func(ctx context.Context) map[placement.Location]bool
	window   *windowController // nil when the capability catalog is missing
	measured *measurementStore // the VRAM footprint overlay (EVB-28)
	log      *slog.Logger

	// Push-style metrics (decisions and waits are events, not state).
	decisions *prometheus.CounterVec   // resourced_lease_decisions_total{decision}
	wait      *prometheus.HistogramVec // resourced_lease_wait_seconds{device}
	fitUp     prometheus.Gauge         // resourced_fit_reading_up
	lastRef   prometheus.Gauge         // resourced_last_refresh_timestamp_seconds
}

func newService(ledger *resources.Ledger, pub Publisher, readings fit.Source, measured *measurementStore, reg *prometheus.Registry, log *slog.Logger) *service {
	s := &service{
		ledger:   ledger,
		pub:      pub,
		readings: readings,
		detect:   placement.Detect,
		measured: measured,
		log:      log,
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "resourced_lease_decisions_total",
			Help: "Lease decisions by status (granted/queued/denied), all time.",
		}, []string{"decision"}),
		wait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "resourced_lease_wait_seconds",
			Help:    "Time a lease spent in the queue before its grant, by device.",
			Buckets: []float64{1, 5, 30, 120, 600, 3600, 86400},
		}, []string{"device"}),
		fitUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "resourced_fit_reading_up",
			Help: "1 if the last live memory reading (nvidia-smi + meminfo) succeeded.",
		}),
		lastRef: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "resourced_last_refresh_timestamp_seconds",
			Help: "Unix time of the ledger's last successful refresh pass.",
		}),
	}
	reg.MustRegister(s.decisions, s.wait, s.fitUp, s.lastRef, ledgerCollector{s})
	return s
}

// apply records one ledger outcome: the decision counter, the wait time
// for a grant that waited, and the matching lease.* event.
func (s *service) apply(ctx context.Context, out resources.Outcome) {
	s.decisions.WithLabelValues(string(out.Status)).Inc()
	switch out.Status {
	case resources.Granted:
		if !out.Lease.QueuedAt.IsZero() {
			s.wait.WithLabelValues(out.Lease.Device).Observe(time.Since(out.Lease.QueuedAt).Seconds())
		}
		s.emit(ctx, queue.LeaseEvent("granted", map[string]any{"lease": out.Lease}))
	case resources.Denied:
		data := map[string]any{"reason": out.Reason}
		if out.Lease.ID != "" {
			data["lease"] = out.Lease
		}
		s.emit(ctx, queue.LeaseEvent("denied", data))
	}
	// Queued emits no event: the request is visible in GET /state until
	// it is granted (event then), withdrawn (DELETE) or denied.
}

func (s *service) emit(ctx context.Context, ev queue.Event) {
	if s.pub == nil {
		return
	}
	if err := s.pub.Publish(ctx, ev); err != nil {
		// Fire-and-forget: a lost lease event is acceptable (events.go).
		s.log.Warn("lease event not published", "key", ev.RoutingKey(), "err", err)
	}
}

// refresh takes one live reading and folds it into the ledger: device
// capacities, the live free RAM (the rule-5 RAM gate), the brain's
// standing GPU lease (measured total−free, so a swap window shows up as
// the lease shrinking to driver overhead), and a FIFO re-evaluation of
// the wait queue.
func (s *service) refresh(ctx context.Context) {
	r, err := s.readings.Readings(ctx)
	if err != nil {
		s.fitUp.Set(0)
		s.log.Warn("live memory reading failed", "err", err)
		return
	}
	s.fitUp.Set(1)
	s.lastRef.SetToCurrentTime()
	present := s.detect(ctx)

	gpu := resources.Device{Name: resources.DeviceGPU, Present: present[placement.GPU],
		VRAMTotalMB: int64(r.TotalVRAMGB * 1024)}
	ramMB := int64(r.TotalRAMGB * 1024)
	s.ledger.SetDevice(gpu)
	s.ledger.SetDevice(resources.Device{Name: resources.DeviceCPU, Present: true,
		RAMTotalMB: ramMB, Cores: runtime.NumCPU()})
	s.ledger.SetDevice(resources.Device{Name: resources.DeviceIGPU, Present: present[placement.IGPU],
		RAMTotalMB: ramMB})
	// Rule 5 (EVB-28): RAM-only workers are grantable any time,
	// verified against the live free RAM — the ledger grants RAM leases
	// only when this reading has room.
	s.ledger.SetLiveFreeRAM(int64(r.FreeRAMGB * 1024))

	if gpu.Present {
		usedMB := int64((r.TotalVRAMGB - r.FreeVRAMGB) * 1024)
		// Co-tenants (EVB-28, rule 5): nvidia-smi's used counts every
		// model on the GPU — the brain AND every capability worker that
		// has loaded. Their declared leases already reserve those bytes,
		// so subtract them before sizing the brain's standing lease;
		// otherwise the ledger double-counts co-tenant usage and can
		// never grant a small worker while a big one runs (the 12 GB
		// image job + 5.5 GB LLM case). Clamped at 0: when the declared
		// budgets exceed the measured usage (workers granted but not
		// loaded yet), the standing lease floors and the ledger errs
		// conservative — free VRAM is never overstated.
		for _, lease := range s.ledger.ActiveLeases() {
			if lease.Device == resources.DeviceGPU && lease.Holder != resources.BrainHolder {
				usedMB -= lease.VRAMMB
			}
		}
		if usedMB < 0 {
			usedMB = 0
		}
		lease, created := s.ledger.RefreshBrainLease(usedMB)
		if created {
			s.log.Info("brain standing lease created", "vram_mb", lease.VRAMMB)
			s.apply(ctx, resources.Outcome{Status: resources.Granted, Lease: lease})
		}
	}
	for _, out := range s.ledger.Reevaluate() {
		s.apply(ctx, out)
	}
}

// ---------------- the HTTP handlers ----------------

// handleLeasePost is POST /lease.
func (s *service) handleLeasePost(w http.ResponseWriter, r *http.Request) {
	var req resources.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("bad lease request: %v", err), http.StatusBadRequest)
		return
	}
	if req.Holder == "" || req.Device == "" {
		http.Error(w, "holder and device are required", http.StatusBadRequest)
		return
	}
	if req.VRAMMB < 0 || req.RAMMB < 0 {
		http.Error(w, "vram_mb and ram_mb must not be negative", http.StatusBadRequest)
		return
	}
	// Measured budget (EVB-28): a capability that has run before and
	// measured bigger than the catalog declares is charged the measured
	// value, not the declaration.
	if req.Capability != "" {
		req.VRAMMB = s.measured.budget(req.Capability, req.Device, req.VRAMMB)
	}
	out := s.ledger.Request(req)
	s.apply(r.Context(), out)
	if out.Status == resources.Granted {
		// Rule 5 (EVB-28): after a grant, recompute the free remainder
		// (total − leases) and run the queue — the invariant is "after
		// every request, every queued lease that fits is granted", and
		// a grant is a request. Under the current policy a grant only
		// shrinks the remainder, so this pass is normally empty; it
		// keeps the invariant event-driven (immediate, not next
		// refresh) and is where a future policy (preemption, leases
		// that fit because another grant changed the mix) lands.
		for _, q := range s.ledger.Reevaluate() {
			s.apply(r.Context(), q)
		}
	}
	switch out.Status {
	case resources.Granted:
		writeJSON(w, http.StatusCreated, map[string]any{"status": out.Status, "lease": out.Lease})
	case resources.Queued:
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status": out.Status, "lease": out.Lease, "position": out.Position,
		})
	default:
		writeJSON(w, http.StatusConflict, map[string]any{"status": out.Status, "reason": out.Reason})
	}
}

// handleLeaseDelete is DELETE /lease/<id>: releases a granted lease or
// withdraws a queued request, then re-evaluates the queue (the freed
// remainder may grant a waiter).
func (s *service) handleLeaseDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/lease/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "want /lease/<id>", http.StatusBadRequest)
		return
	}
	lease, ok := s.ledger.Release(id)
	if !ok {
		http.Error(w, fmt.Sprintf("no such lease %q", id), http.StatusNotFound)
		return
	}
	s.emit(r.Context(), queue.LeaseEvent("released", map[string]any{"lease": lease}))
	for _, out := range s.ledger.Reevaluate() {
		s.apply(r.Context(), out)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "released", "lease": lease})
}

func (s *service) handleState(w http.ResponseWriter, r *http.Request) {
	st := s.ledger.State()
	// The top-level keys are the broker contract (capworker parses
	// devices/leases/queued); the window rides along as an extra field.
	resp := map[string]any{
		"devices":       st.Devices,
		"leases":        st.Leases,
		"queued":        st.Queued,
		"stream_active": st.StreamActive, // the game-watchdog hold (EVB-29)
	}
	if s.window != nil {
		resp["window"] = s.window.State()
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// ---------------- metrics ----------------

// ledgerCollector exposes the ledger's current state as gauges at scrape
// time: leases per device (count and MB) and the queue depth — pull from
// the ledger, no bookkeeping to drift.
type ledgerCollector struct{ s *service }

func (c ledgerCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("resourced_leases_active", "Active leases per device (includes the brain standing lease).", []string{"device"}, nil)
	ch <- prometheus.NewDesc("resourced_leases_vram_mb", "VRAM held by active leases, per device.", []string{"device"}, nil)
	ch <- prometheus.NewDesc("resourced_leases_ram_mb", "System RAM held by active leases, per device (all devices draw the shared pool).", []string{"device"}, nil)
	ch <- prometheus.NewDesc("resourced_queue_depth", "Leases waiting in the FIFO queue.", nil, nil)
	ch <- prometheus.NewDesc("resourced_device_present", "1 if the device is present on this box (live probe).", []string{"device"}, nil)
	ch <- prometheus.NewDesc("resourced_device_vram_total_mb", "The device's total VRAM in MB (live reading).", []string{"device"}, nil)
	ch <- prometheus.NewDesc("resourced_device_ram_total_mb", "The device's total system RAM in MB (live reading).", []string{"device"}, nil)
}

func (c ledgerCollector) Collect(ch chan<- prometheus.Metric) {
	st := c.s.ledger.State()
	devs := map[string]resources.Device{}
	for _, d := range st.Devices {
		devs[d.Name] = d
		ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("resourced_device_present", "", []string{"device"}, nil), prometheus.GaugeValue, boolFloat(d.Present), d.Name)
		ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("resourced_device_vram_total_mb", "", []string{"device"}, nil), prometheus.GaugeValue, float64(d.VRAMTotalMB), d.Name)
		ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("resourced_device_ram_total_mb", "", []string{"device"}, nil), prometheus.GaugeValue, float64(d.RAMTotalMB), d.Name)
	}
	count, vram, ram := map[string]int{}, map[string]int64{}, map[string]int64{}
	for _, l := range st.Leases {
		count[l.Device]++
		vram[l.Device] += l.VRAMMB
		ram[l.Device] += l.RAMMB
	}
	for name := range devs {
		ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("resourced_leases_active", "", []string{"device"}, nil), prometheus.GaugeValue, float64(count[name]), name)
		ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("resourced_leases_vram_mb", "", []string{"device"}, nil), prometheus.GaugeValue, float64(vram[name]), name)
		ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("resourced_leases_ram_mb", "", []string{"device"}, nil), prometheus.GaugeValue, float64(ram[name]), name)
	}
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("resourced_queue_depth", "", nil, nil), prometheus.GaugeValue, float64(len(st.Queued)))
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// ---------------- main ----------------

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// buildWindowController wires the production window controller (EVB-27,
// revocation EVB-30): the catalog's brain-out capabilities, the queue
// depths (AMQP ready + management API unacked), the local jobs from the
// agent job store, jarvis-swap as the SwapRunner and the ledger's
// filtered revocation (the window close gives the workers' VRAM back to
// the brain that is coming home). It returns nil when the capability
// catalog is missing or the broker URL has no host — the ledger keeps
// serving leases without windows.
func buildWindowController(conn *queue.Conn, pub Publisher, reg *prometheus.Registry, log *slog.Logger, ledger *resources.Ledger) *windowController {
	catalogPath := envOr("CAPABILITIES_FILE", "config/capabilities.yaml")
	cat, err := catalog.Load(catalogPath)
	if err != nil {
		log.Error("capability catalog — the swap window controller is disabled", "file", catalogPath, "err", err)
		return nil
	}
	amqpURL := queue.AMQPURL()
	base, user, err := mgmtBase(amqpURL)
	if err != nil {
		log.Error("management API base — the swap window controller is disabled", "url", amqpURL, "err", err)
		return nil
	}
	// The management API's queue routes are per-vhost; the amqp URL's
	// path is the vhost (empty = "/").
	vhost := "/"
	if u, uerr := url.Parse(amqpURL); uerr == nil {
		if v := strings.TrimPrefix(u.Path, "/"); v != "" {
			vhost = v
		}
	}
	mgmt := &mgmtClient{base: base, user: user, client: &http.Client{Timeout: 10 * time.Second}}
	caps := brainOutCaps(cat)
	def := resources.DefaultWindowConfig()
	cfg := resources.WindowConfig{
		MaxWindow:    durEnv("MAX_WINDOW", def.MaxWindow, log),
		MaxBrainWait: durEnv("MAX_BRAIN_WAIT", def.MaxBrainWait, log),
		MinWindow:    durEnv("MIN_WINDOW", def.MinWindow, log),
	}
	log.Info("swap window controller wired", "caps", caps,
		"max_window", cfg.MaxWindow.String(),
		"max_brain_wait", cfg.MaxBrainWait.String(),
		"min_window", cfg.MinWindow.String())
	return newWindowController(
		cfg,
		newShellSwapRunner(envOr("SWAP_CMD", "jarvis-swap"), os.Getenv("SWAP_STATE_FILE"), 0, log),
		caps,
		cat.SortedNames(), // the /state depth snapshot covers the whole catalog (EVB-30)
		func(ctx context.Context, name string) (int, error) { return conn.QueueDepth(name) },
		func(ctx context.Context) (map[string]int, error) { return mgmt.unacked(ctx, vhost) },
		func(ctx context.Context) (int, []resources.ReadyJob, error) {
			store, err := agentjob.Open(agentjob.DataDir())
			if err != nil {
				return 0, nil, err
			}
			return localJobs(store, log)
		},
		func(caps []string) []resources.Lease { return ledger.RevokeGPULeasesFor(caps) },
		pub,
		reg,
		log,
	)
}

// defaultMeasurementsFile is the VRAM footprint overlay's location
// (EVB-28) — the state dir, next to the swap-window file.
func defaultMeasurementsFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "cap-measurements.json"
	}
	return filepath.Join(home, ".local", "state", "jarvis", "cap-measurements.json")
}

// durEnv reads a duration env var, falling back to def when unset and
// exiting on a value that cannot be a positive duration (a typo in the
// unit file must not silently change the window policy).
func durEnv(key string, def time.Duration, log *slog.Logger) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Error("bad duration env", "key", key, "value", v)
		os.Exit(1)
	}
	return d
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	listen := envOr("LISTEN_ADDR", ":9104")
	interval, err := time.ParseDuration(envOr("REFRESH_INTERVAL", "30s"))
	if err != nil || interval <= 0 {
		log.Error("bad REFRESH_INTERVAL", "value", os.Getenv("REFRESH_INTERVAL"))
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The broker is required: without it the lease.* events would be
	// silent and the dashboard would not see the ledger at all.
	conn, err := queue.NewConn()
	if err != nil {
		log.Error("rabbitmq", "err", err)
		os.Exit(1)
	}
	defer conn.Close()
	pub, err := queue.NewEventPublisher(conn, "resourced")
	if err != nil {
		log.Error("events publisher", "err", err)
		os.Exit(1)
	}
	defer pub.Close()

	// Devices start absent; the first refresh fills them in. A failed
	// startup reading leaves the API up (it denies until the next
	// successful refresh) rather than deadlocking the box.
	ledger := resources.New(
		resources.Device{Name: resources.DeviceGPU, Present: false},
		resources.Device{Name: resources.DeviceCPU, Present: false},
		resources.Device{Name: resources.DeviceIGPU, Present: false},
	)
	reg := prometheus.NewRegistry()
	measured := newMeasurementStore(envOr("MEASUREMENTS_FILE", defaultMeasurementsFile()), log)
	svc := newService(ledger, pub, fit.Live{}, measured, reg, log)
	svc.refresh(ctx) // first pass before the API takes traffic

	// The swap-window controller (EVB-27, plan §3.6 rules 3–4). It
	// needs the capability catalog to know which capability queues are
	// the brain-out ones; without it the ledger still serves leases,
	// the windows just never open.
	windowInterval := durEnv("WINDOW_INTERVAL", 30*time.Second, log)
	svc.window = buildWindowController(conn, pub, reg, log, ledger)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /lease", svc.handleLeasePost)
	mux.HandleFunc("DELETE /lease/", svc.handleLeaseDelete)
	mux.HandleFunc("GET /state", svc.handleState)
	mux.HandleFunc("POST /measure", svc.handleMeasurePost)
	mux.HandleFunc("GET /measured", svc.handleMeasuredGet)
	mux.HandleFunc("DELETE /measured/", svc.handleMeasuredDelete)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	srv := &http.Server{Addr: listen, Handler: mux}
	go func() {
		log.Info("resourced listening", "addr", listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
			stop()
		}
	}()

	// The window controller ticks on its own goroutine: an open or a
	// close takes minutes (model load both ways) and must never block
	// the ledger's refresh loop.
	if svc.window != nil {
		go windowTickLoop(ctx, svc.window, windowInterval)
	}

	// The game/stream watchdog (EVB-29, plan §3.6 rule 2): while a GAME
	// is active the ledger revokes the capability workers' GPU leases,
	// holds the GPU queue, and resourced stops the brain (jarvis-swap
	// out --game; the 60s keepalive re-arms the maintenance window and
	// retries the brain stop past the round guard). A desktop stream
	// takes no GPU action. Its own goroutine — the window operations
	// block for the model loads.
	go newStreamController(svc, reg, log).Run(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			srv.Shutdown(shutdownCtx)
			cancel()
			return
		case <-ticker.C:
			svc.refresh(ctx)
		}
	}
}
