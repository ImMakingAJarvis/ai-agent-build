// The swap-window controller (plan §3.6, rules 3–4; task EVB-27). The
// policy itself is pure and lives in internal/resources (DecideWindow);
// this file collects the facts it decides on and acts on the decision:
//
//   - the capability queue depths — the AMQP ready count plus the
//     management API's unacked count (a capworker holds its message
//     unacked from pickup until the Result is confirmed, so ready
//     alone would call a draining window "empty" mid-job);
//   - the local (brain) jobs from the agent job store — a running one
//     is a working brain (rule 3), a ready one with its wait is the
//     close condition (rule 4);
//   - the swap state jarvis-swap keeps in its window state file.
//
// The window is opened and closed through jarvis-swap (the SwapRunner):
// everything docker/nvidia-smi/watchdog-shaped stays in that one script.
// The controller ticks on its own goroutine so a swap (minutes of model
// load both ways) never blocks the ledger's refresh loop.
//
// Photo work is no special case here: image.generate is just another
// catalog capability whose gpu placement has needs_brain_out, so its
// queue is drained like any other (the old agent-run photo bracket is
// gone — EVB-27).
//
// Invariant 1 holds by construction: the controller only ever runs in
// resourced, which ticks between rounds — a running local job (rule 3)
// is read from the same job store agent-run writes, and the round guard
// inside jarvis-swap is the second line if the two ever disagree.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/catalog"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/resources"
)

// ---------------- the swap runner ----------------

// SwapRunner opens and closes the brain swap window. The production
// implementation shells out to jarvis-swap (it owns docker, nvidia-smi
// and the watchdog maintenance window); tests use a fake.
type SwapRunner interface {
	// Open runs `jarvis-swap out --for <caps> --reason <r> --force`
	// and blocks until the window is fully open (brain stopped, art
	// loaded). It never bypasses a running round — the guard is inside
	// jarvis-swap (invariant 1).
	Open(ctx context.Context, forCaps []string, reason string) error
	// Close runs `jarvis-swap in --reason <r>` and blocks until the
	// brain is back (started, healthy, real completion check).
	Close(ctx context.Context, reason string) error
	// State reports the window marker jarvis-swap keeps while the
	// window is open (absent file = closed).
	State() (open bool, forCaps []string, openedAt time.Time, err error)
}

// shellSwapRunner is the production SwapRunner: jarvis-swap on the host.
// resourced runs as a host systemd service for exactly this reason (the
// old compose container could not reach the host's docker).
type shellSwapRunner struct {
	cmd       string
	statePath string
	timeout   time.Duration
	log       *slog.Logger
}

func newShellSwapRunner(cmd, statePath string, timeout time.Duration, log *slog.Logger) *shellSwapRunner {
	if cmd == "" {
		cmd = "jarvis-swap"
	}
	if statePath == "" {
		home, _ := os.UserHomeDir()
		statePath = filepath.Join(home, ".local", "state", "jarvis", "swap-window.json")
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute // jarvis-swap's own steps are far shorter; this only catches a hang
	}
	return &shellSwapRunner{cmd: cmd, statePath: statePath, timeout: timeout, log: log}
}

func (r *shellSwapRunner) Open(ctx context.Context, forCaps []string, reason string) error {
	return r.run(ctx, []string{"out", "--for", strings.Join(forCaps, ","), "--reason", reason, "--force"}...)
}

func (r *shellSwapRunner) Close(ctx context.Context, reason string) error {
	return r.run(ctx, "in", "--reason", reason)
}

func (r *shellSwapRunner) run(ctx context.Context, args ...string) error {
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	c := exec.CommandContext(cctx, r.cmd, args...)
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s", r.timeout)
	}
	out := tail(buf.String())
	if err != nil {
		// The journal gets the tail of jarvis-swap's own output so a
		// failed swap is diagnosable without tailing its log file.
		return fmt.Errorf("%s %s: %v (%s)", r.cmd, strings.Join(args, " "), err, out)
	}
	r.log.Info("window: jarvis-swap finished", "cmd", r.cmd, "args", strings.Join(args, " "), "output", out)
	return nil
}

// State reads the window state file ({"for":[...],"reason","opened_at"}).
// A marker without a parseable opened_at still reports open=true —
// DecideWindow closes that as stale, so a hand-written or truncated file
// self-heals instead of stranding the controller.
func (r *shellSwapRunner) State() (bool, []string, time.Time, error) {
	b, err := os.ReadFile(r.statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil, time.Time{}, nil
	}
	if err != nil {
		return false, nil, time.Time{}, err
	}
	var st struct {
		For      []string `json:"for"`
		Reason   string   `json:"reason"`
		OpenedAt string   `json:"opened_at"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return false, nil, time.Time{}, fmt.Errorf("parse %s: %w", r.statePath, err)
	}
	t, perr := time.Parse(time.RFC3339, st.OpenedAt)
	if perr != nil {
		return true, st.For, time.Time{}, nil
	}
	return true, st.For, t, nil
}

// tail keeps the last few hundred characters of a command's output.
func tail(s string) string {
	if len(s) > 300 {
		s = "…" + s[len(s)-300:]
	}
	return strings.TrimSpace(s)
}

// ---------------- the facts ----------------

// mgmtClient reads per-queue counts from the RabbitMQ management API
// (port 15672). The AMQP ready count (queue.Conn.QueueDepth) misses the
// in-flight messages a capworker holds unacked until its Result is
// confirmed — the unacked count lives only in the management API.
type mgmtClient struct {
	base   string
	user   *url.Userinfo
	client *http.Client
}

// mgmtBase derives the management API base from the amqp URL: same host
// and credentials, the management listener on port 15672.
func mgmtBase(amqpURL string) (string, *url.Userinfo, error) {
	u, err := url.Parse(amqpURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse %q: %w", amqpURL, err)
	}
	if u.Hostname() == "" {
		return "", nil, fmt.Errorf("no host in %q", amqpURL)
	}
	user := u.User
	u.Scheme = "http"
	u.Host = net.JoinHostPort(u.Hostname(), "15672")
	u.User = nil // the client carries the credentials (SetBasicAuth)
	u.Path = ""
	u.RawQuery = ""
	return u.String(), user, nil
}

// unacked returns each queue's unacked count (queue name → count).
func (m *mgmtClient) unacked(ctx context.Context, vhost string) (map[string]int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		m.base+"/api/queues/"+url.PathEscape(vhost), nil)
	if err != nil {
		return nil, err
	}
	if m.user != nil {
		pass, _ := m.user.Password()
		req.SetBasicAuth(m.user.Username(), pass)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("management API: %s", resp.Status)
	}
	var qs []struct {
		Name                   string `json:"name"`
		MessagesUnacknowledged int    `json:"messages_unacknowledged"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&qs); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(qs))
	for _, q := range qs {
		out[q.Name] = q.MessagesUnacknowledged
	}
	return out, nil
}

// localDriver reports whether a job's driver is the local (brain) one.
// The default is local: driver "" means "no other driver was chosen".
func localDriver(d string) bool { return d == "" || d == "local" }

// localJobs counts the local (brain) jobs the policy needs: the running
// ones (rule 3 — a working brain is never evicted) and the ready ones
// with their ready times (rule 4's close condition). The ready time is
// the state file's mtime — the store rewrites state.json on every
// status transition, so for a ready job it is when it became ready.
// A ready job without a state file is skipped (not counted): guessing
// its wait early could close a window it should not close.
//
// Rule 4 counts only ready jobs the brain could ACTUALLY run — the same
// deps gate as Next (DepsMet). The store fills with dep-blocked backlog
// jobs (dozens, days old); counting their wait would close every window
// on the first tick after it opens (oldest ready state.json >> 20 m),
// before the GPU work that triggered it has even started. A dep-blocked
// job is not served by the brain coming back — its wait is on its
// dependencies, not on the window.
//
// Photo-tagged jobs are NOT brain jobs even though they run with a local
// driver: the executor is code (jarvis-photo) that publishes the
// image.generate request and blocks for the Result — the work the window
// exists for. Counting a running photo job as a working brain would
// deadlock the window (the job waits for the Result; the Result waits
// for the window), and counting a ready photo job's wait as a brain wait
// would close the window the photo work needs. Their load is already
// visible in the cap.image.generate depth.
func localJobs(store *agentjob.Store, log *slog.Logger) (int, []resources.ReadyJob, error) {
	all, err := store.List()
	if err != nil {
		return 0, nil, err
	}
	status := make(map[string]string, len(all))
	for _, j := range all {
		status[j.ID] = j.Status
	}
	running := 0
	var ready []resources.ReadyJob
	for _, j := range all {
		if !localDriver(j.Driver) || j.HasTag(agentjob.TagPhoto) {
			continue
		}
		switch j.Status {
		case agentjob.Running:
			running++
		case agentjob.Ready:
			if !agentjob.DepsMet(j, status) {
				continue // dep-blocked: not a brain wait
			}
			fi, err := os.Stat(filepath.Join(store.JobDir(j.ID), "state.json"))
			if err != nil {
				log.Warn("window: ready job has no state file — not counting its wait", "job", j.ID, "err", err)
				continue
			}
			ready = append(ready, resources.ReadyJob{ID: j.ID, ReadySince: fi.ModTime()})
		}
	}
	return running, ready, nil
}

// brainOutCaps lists, sorted, the capabilities whose gpu placement needs
// the brain out of the 4090 (the catalog's needs_brain_out) — the queues
// the window opens for and drains. Photo work is among them (the
// image.generate gpu placement), exactly like any other GPU capability.
func brainOutCaps(cat *catalog.File) []string {
	seen := map[string]bool{}
	var out []string
	for name, c := range cat.Capabilities {
		w := c.WorkerAt("gpu")
		if w != nil && w.NeedsBrainOut && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------- the controller ----------------

// windowInfo is the "window" field of GET /state: the last known shape
// of the window and the last decision's reason (why it holds, or why it
// just closed). CapDepths is the per-capability queue depth snapshot
// (EVB-30, the dashboard's Machine view): ready count plus unacked, per
// catalog capability name — the AMQP side of "what is waiting for the
// machine".
type windowInfo struct {
	Open      bool          `json:"open"`
	For       []string      `json:"for,omitempty"`
	OpenedAt  time.Time     `json:"opened_at,omitempty"`
	Reason    string        `json:"reason,omitempty"`
	LastTick  time.Time     `json:"last_tick,omitempty"`
	CapDepths map[string]int `json:"cap_depths,omitempty"`
}

// windowController owns one window's lifecycle: it ticks on its own
// schedule, collects the facts, asks DecideWindow and acts through the
// SwapRunner. The policy stays in internal/resources; everything here
// is collection and side effects.
type windowController struct {
	cfg  resources.WindowConfig
	swap SwapRunner
	caps      []string // the watched (brain-out) capability names, for the decision
	depthCaps []string // every catalog capability — the /state depth snapshot

	depth   func(ctx context.Context, queueName string) (int, error)          // AMQP ready count
	unacked func(ctx context.Context) (map[string]int, error)                 // mgmt, queue name → count
	jobs    func(ctx context.Context) (int, []resources.ReadyJob, error)      // running, ready local
	pub     Publisher
	log     *slog.Logger

	openGauge   prometheus.Gauge       // resourced_window_open
	ageGauge    prometheus.Gauge       // resourced_window_age_seconds
	opensTotal  *prometheus.CounterVec // resourced_window_opens_total{trigger}
	closesTotal *prometheus.CounterVec // resourced_window_closes_total{reason}

	swapMu sync.Mutex // serializes every jarvis-swap operation (see openWindow/closeWindow)
	mu     sync.Mutex
	last   windowInfo
}

func newWindowController(
	cfg resources.WindowConfig,
	swap SwapRunner,
	caps []string,
	depthCaps []string,
	depth func(ctx context.Context, queueName string) (int, error),
	unacked func(ctx context.Context) (map[string]int, error),
	jobs func(ctx context.Context) (int, []resources.ReadyJob, error),
	pub Publisher,
	reg *prometheus.Registry,
	log *slog.Logger,
) *windowController {
	wc := &windowController{
		cfg: cfg, swap: swap, caps: caps, depthCaps: depthCaps,
		depth: depth, unacked: unacked, jobs: jobs,
		pub: pub, log: log,
		openGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "resourced_window_open",
			Help: "1 while the brain swap window is open.",
		}),
		ageGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "resourced_window_age_seconds",
			Help: "Age of the open swap window in seconds (0 when closed).",
		}),
		opensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "resourced_window_opens_total",
			Help: "Swap windows opened by the controller, by trigger.",
		}, []string{"trigger"}),
		closesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "resourced_window_closes_total",
			Help: "Swap windows closed by the controller, by close class.",
		}, []string{"reason"}),
	}
	reg.MustRegister(wc.openGauge, wc.ageGauge, wc.opensTotal, wc.closesTotal)
	return wc
}

// tick runs one controller pass: collect the facts, decide, act. A
// failed fact-collection holds the tick (the last known state stands);
// a failed swap is retried on the next tick.
func (wc *windowController) tick(ctx context.Context) {
	open, forCaps, openedAt, err := wc.swap.State()
	if err != nil {
		wc.log.Warn("window: cannot read the swap state — holding the tick", "err", err)
		return
	}
	// The depth snapshot covers every catalog capability (EVB-30: the
	// dashboard's per-capability depth comes from here, not from a
	// second management-API call); the decision's watched set is the
	// brain-out subset of it, so each queue is read once per tick.
	snap := make(map[string]int, len(wc.depthCaps))
	for _, cap := range wc.depthCaps {
		name := queue.CapQueueName(cap)
		n, err := wc.depth(ctx, name)
		if err != nil {
			wc.log.Warn("window: queue depth failed — holding the tick", "queue", name, "err", err)
			return
		}
		snap[cap] = n
	}
	uaKnown := true
	if ua, err := wc.unacked(ctx); err != nil {
		uaKnown = false
		// Without the unacked counts the drained check cannot tell
		// "empty" from "one delivery in flight", so this tick refuses
		// to close on a drained decision (below). The window then
		// closes at max_window or on the brain wait, whichever first —
		// both safe. In practice the management API and AMQP are the
		// same process, so this path is rare.
		wc.log.Warn("window: management API unreachable — no drained close this tick", "err", err)
	} else {
		for cap := range snap {
			snap[cap] += ua[queue.CapQueueName(cap)]
		}
	}
	depth := make(map[string]int, len(wc.caps))
	for _, cap := range wc.caps {
		depth[cap] = snap[cap]
	}
	running, ready, err := wc.jobs(ctx)
	if err != nil {
		wc.log.Warn("window: job store failed — holding the tick", "err", err)
		return
	}

	d := resources.DecideWindow(wc.cfg, resources.WindowInput{
		WindowOpen:   open,
		OpenedAt:     openedAt,
		Now:          time.Now(),
		CapDepth:     depth,
		RunningLocal: running,
		ReadyLocal:   ready,
	})
	// A drained close with the unacked counts unknown is a guess: the
	// ready count says zero but a delivery may still be in flight.
	// Hold — the window closes at max_window or on the brain wait,
	// both of which are safe closes.
	if !uaKnown && d.Action == resources.WindowClose && strings.HasPrefix(d.Reason, "drained") {
		d = resources.WindowDecision{
			Action: resources.WindowHold,
			Reason: d.Reason + " (in-flight unknown: management API down)",
		}
	}
	now := time.Now().UTC()
	wc.mu.Lock()
	wc.last = windowInfo{Open: open, For: forCaps, OpenedAt: openedAt, Reason: d.Reason, LastTick: now, CapDepths: snap}
	wc.mu.Unlock()
	wc.openGauge.Set(boolFloat(open))
	if open && !openedAt.IsZero() {
		wc.ageGauge.Set(time.Since(openedAt).Seconds())
	} else {
		wc.ageGauge.Set(0)
	}
	switch d.Action {
	case resources.WindowOpen:
		wc.log.Info("window: opening", "for", d.For, "reason", d.Reason)
		if err := wc.openWindow(ctx, d.For, d.Reason, "queued"); err != nil {
			wc.log.Error("window: open failed — retrying next tick", "err", err)
			return
		}
	case resources.WindowClose:
		wc.log.Info("window: closing", "reason", d.Reason)
		if err := wc.closeWindow(ctx, d.Reason, closeClass(d.Reason)); err != nil {
			wc.log.Error("window: close failed — retrying next tick", "err", err)
			return
		}
	}
}

// openWindow runs one `jarvis-swap out` and records it (metric +
// window.opened event). Every swap operation goes through swapMu so the
// game watchdog's close (CloseForGame) and a tick's open/close can never
// run jarvis-swap concurrently — a double `in` would race the brain's
// health check.
func (wc *windowController) openWindow(ctx context.Context, forCaps []string, reason, trigger string) error {
	wc.swapMu.Lock()
	defer wc.swapMu.Unlock()
	if err := wc.swap.Open(ctx, forCaps, reason); err != nil {
		return err
	}
	wc.opensTotal.WithLabelValues(trigger).Inc()
	wc.publish(ctx, queue.WindowEvent("opened", map[string]any{"for": forCaps, "reason": reason}))
	return nil
}

// closeWindow runs one `jarvis-swap in` and records it. It re-checks the
// window state under swapMu: when another path closed the window first
// (the game watchdog racing a drained tick), the goal is already met and
// the call is a no-op — a second `jarvis-swap in` against a running
// brain is what the mutex exists to prevent.
func (wc *windowController) closeWindow(ctx context.Context, reason, cls string) error {
	wc.swapMu.Lock()
	defer wc.swapMu.Unlock()
	open, forCaps, _, err := wc.swap.State()
	if err != nil {
		return err
	}
	if !open {
		wc.log.Info("window: already closed (another path won the race) — skipping", "reason", reason)
		return nil
	}
	if err := wc.swap.Close(ctx, reason); err != nil {
		return err
	}
	wc.closesTotal.WithLabelValues(cls).Inc()
	wc.publish(ctx, queue.WindowEvent("closed", map[string]any{"for": forCaps, "reason": reason}))
	wc.mu.Lock()
	wc.last = windowInfo{Reason: "closed: " + reason, LastTick: time.Now().UTC()}
	wc.mu.Unlock()
	return nil
}

// CloseForGame is the game watchdog's window path (plan §3.6 rule 2,
// EVB-29): the stream needs the 4090, so an open swap window closes now
// and the brain comes back before the game gets its VRAM — the same
// order as a drained close, for a game reason. A no-op when no window
// is open. The caller (the stream controller) holds the GPU lease
// revocation and the stream hold around this, so by the time the brain
// is back there is no capability-worker VRAM left to wait for.
func (wc *windowController) CloseForGame(ctx context.Context, reason string) error {
	return wc.closeWindow(ctx, reason, "game")
}

func (wc *windowController) publish(ctx context.Context, ev queue.Event) {
	if wc.pub == nil {
		return
	}
	if err := wc.pub.Publish(ctx, ev); err != nil {
		wc.log.Warn("window event not published", "key", ev.RoutingKey(), "err", err)
	}
}

// State is the window's last known shape (the "window" field of /state).
func (wc *windowController) State() windowInfo {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	return wc.last
}

// closeClass buckets a close reason for the metric label (a bounded
// set, not free text).
func closeClass(reason string) string {
	switch {
	case strings.HasPrefix(reason, "max window"):
		return "max_window"
	case strings.HasPrefix(reason, "brain job"):
		return "brain_wait"
	case strings.HasPrefix(reason, "drained"):
		return "drained"
	case strings.HasPrefix(reason, "stale"):
		return "stale"
	default:
		return "other"
	}
}

// windowTickLoop runs the controller off the ledger's refresh loop: an
// open or a close takes minutes (model load both ways) and the refresh
// must keep running through it. The first tick happens at startup so a
// restarted resourced adopts a window that is already open (its state
// file survived the restart) instead of double-opening.
func windowTickLoop(ctx context.Context, wc *windowController, interval time.Duration) {
	wc.tick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			wc.tick(ctx)
		}
	}
}
