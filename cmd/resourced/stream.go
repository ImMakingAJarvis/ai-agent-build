// The game/stream watchdog (plan §3.6 rule 2, task EVB-29): "the game
// watchdog wins — when a game is active, free the 4090 for it". The
// rule is per KIND of stream (IsGame):
//
//   - a Steam game (local, or Steam Remote Play — the game runs on this
//     host) needs the 4090's VRAM, so the brain goes OUT: the ledger's
//     GPU hold turns on, every capability worker's GPU lease is
//     revoked (task-26 drain) and the swap window opens for the game
//     (jarvis-swap out --game: brain stopped, for:["game"], NO photo
//     service — the game wants the VRAM the art model would take).
//     When the game ends, the window closes (only if it is the
//     game's) and the queued GPU leases grant again.
//   - a Sunshine stream on this box is plain DESKTOP streaming: it
//     needs only NVENC (a few hundred MB of VRAM) and coexists with the
//     brain and the capability workers, so it takes NO GPU action — no
//     hold, no revocation, no window. It still logs, sets the gauge and
//     emits stream.started{game:false} for the dashboard.
//
// resourced runs its own streamwatcher (the same package and detection
// the other host consumers use: a /proc-cmdline scan for the Steam game
// install path, Sunshine over /proc or its API, NVENC encoder
// utilization via nvidia-smi as the fallback that always works from a
// host service).
//
// The brain side is resourced's: the old worker-container watchdog was
// blind (no docker socket, no host /proc view) and is gone. resourced
// owns the jarvis-swap call and the game keepalive:
//
//   - while a round runs, jarvis-swap's round guard aborts the brain
//     stop (invariant 1). Waiting it out is not an option when the
//     round outlives the session — the 2026-11-18 black stream: the
//     round ran 25 minutes, the game 10, and the brain never released
//     its VRAM. So the game CUTS the round: it writes the same
//     restart-signal the !photonow preemption uses, the runner kills
//     the round within a second and the job goes back to ready (it
//     re-runs in a fresh conversation once the game is over). One
//     Discord line tells the owner.
//   - while the game wants the 4090 (brain-out pending, brain out,
//     brain coming back), a marker file (game-hold) keeps `jarvis-jobs
//     next` from starting ANY new local round: the cut round is back
//     in the ready queue in a second and would otherwise race the
//     brain-out (start, guard abort, cut, start, ...). The engine-down
//     skip covers the game itself; the marker covers the gap around it.
//   - while the brain is still up (brain-out pending) the keepalive
//     ticks every 5 s — the brain goes out the moment it legally may;
//     in the steady state (brain out) it goes back to 60 s.
//   - the script arms a 4 h watchdog-maintenance window that keeps
//     jarvis-watchdog from restarting the brain; the keepalive
//     re-arms it for the life of the game (long sessions outrun 4 h).
//
// The revoked cap workers notice on their next /state poll (LeasePollS,
// default 30 s) and run the task-26 drain: finish the in-flight
// delivery, cancel the consumer, take no new work — no half-written
// artifacts, no lost messages (invariant 9). Their models stay loaded
// until the idle-unload (IdleS), so the game's VRAM lands within a
// minute or two of the last in-flight job.
//
// CPU and iGPU leases are never touched — the rule is GPU-only (a game
// does not compete for the box's RAM).
//
// The jobs-lane half of the old pause is the task-runner's: it pauses
// the brain jobs' GPU queue (jobs.gpu) for game streams too — a game
// stream means a game is running, and the GPU jobs queued behind it
// would not run anyway.
//
// Config (env, all optional):
//
//	SUNSHINE_ENDPOINT     Sunshine API base (e.g. http://127.0.0.1:8080/api/v2).
//	STEAM_ENDPOINT        Steam in-home streaming status page.
//	SUNSHINE_STREAM_PORT  the UDP stream port Sunshine listens on (48010).
//	STEAM_GAME_EXCLUDES   comma-separated command-line substrings that opt
//	                      a game out of the brain stop (a light game the
//	                      owner does not want to evict the brain for).
//	BRAIN_URL             the brain engine's HTTP endpoint the keepalive
//	                      probes (default http://127.0.0.1:11434).

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/streamwatcher"
)

// streamController owns the watchdog: the watcher's transitions and the
// ledger/window actions they trigger. It runs on its own goroutine (a
// swap blocks for the engine load, minutes) and never blocks the
// ledger's refresh loop.
type streamController struct {
	svc *service
	log *slog.Logger

	active prometheus.Gauge // resourced_stream_active

	mu         sync.Mutex // guards gameActive and the game-hold marker
	gameActive bool       // a game-kind stream is running (hold on, keepalive armed)

	preemptNotified bool // the owner got the "I cut your round" line this session

	// Test-overridable probes: the brain liveness check and the
	// watchdog-maintenance status/re-arm (window.go owns the defaults).
	probeBrain    func(ctx context.Context) bool
	maintainAlive func() bool
	maintainRearm func() error

	// Test-overridable preemption side effects (the defaults shell out
	// on the host; the tests point them at temp dirs and recorders).
	localRounds func() []string    // driver-local rounds in flight (jarvis-jobs)
	notifyOwner func(string) error // the one Discord line per game session
	signalDir   string             // dir holding restart-signal (JARVIS_CONF)
	gameHold    string             // the game-hold marker (jarvis-jobs next's gate)
	sweepEvery  time.Duration      // brain-back sweep interval (default 30 s)
}

func newStreamController(svc *service, reg *prometheus.Registry, log *slog.Logger) *streamController {
	sc := &streamController{svc: svc, log: log}
	sc.active = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "resourced_stream_active",
		Help: "1 while a stream (Sunshine/Steam) is active; game streams additionally hold the GPU and stop the brain.",
	})
	sc.probeBrain = httpBrainUp
	sc.maintainAlive = func() bool {
		out, err := exec.Command("jarvis-watchdog", "maintain", "status").CombinedOutput()
		return err == nil && strings.Contains(string(out), "maintenance active")
	}
	sc.maintainRearm = func() error {
		_, err := exec.Command("jarvis-watchdog", "maintain", "4h", "steam game active").CombinedOutput()
		return err
	}
	sc.localRounds = localRoundsRunning
	sc.notifyOwner = func(msg string) error {
		_, err := exec.Command("jarvis-discord", "notify", msg).CombinedOutput()
		return err
	}
	if d := os.Getenv("JARVIS_CONF"); d != "" {
		sc.signalDir = d
	} else if home, err := os.UserHomeDir(); err == nil {
		sc.signalDir = filepath.Join(home, ".config", "jarvis")
	}
	if p := os.Getenv("JARVIS_GAME_HOLD_FILE"); p != "" {
		sc.gameHold = p
	} else if home, err := os.UserHomeDir(); err == nil {
		sc.gameHold = filepath.Join(home, ".local", "state", "jarvis", "game-hold")
	}
	sc.sweepEvery = 30 * time.Second
	reg.MustRegister(sc.active)
	return sc
}

// Run starts the watcher and processes its state transitions until ctx
// is done. Start emits the initial state as well, so a resourced that
// boots mid-stream takes the game path immediately (the watcher's first
// StateChange is the current state, not just later transitions). The
// Adaptive timer drives the game keepalive (brain-out retries,
// maintenance re-arm): 5 s while the brain is still up or the window is
// not the game's yet, 60 s in the steady state — the interval
// gameKeepalive returns. The stream transitions are event-driven; this
// is the only periodic part.
func (sc *streamController) Run(ctx context.Context) {
	cfg := streamwatcher.Config{
		SunshineEndpoint: os.Getenv("SUNSHINE_ENDPOINT"),
		SteamEndpoint:    os.Getenv("STEAM_ENDPOINT"),
	}
	if n, err := strconv.Atoi(os.Getenv("SUNSHINE_STREAM_PORT")); err == nil && n > 0 {
		cfg.SunshineStreamPort = n
	}
	cfg.SteamGameExcludes = splitCSV(os.Getenv("STEAM_GAME_EXCLUDES"))
	watch := streamwatcher.New(cfg)
	ch := make(chan streamwatcher.StateChange, 1)
	watch.Start(ch)
	sc.log.Info("stream watchdog started (game → hold the GPU + stop the brain; desktop → no action)",
		"game_excludes", cfg.SteamGameExcludes)
	keepalive := time.NewTimer(60 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case change := <-ch:
			if change.Stream != streamwatcher.NoStream {
				sc.streamStarted(ctx, change.Stream)
			} else {
				sc.streamStopped(ctx)
			}
		case <-keepalive.C:
			keepalive.Reset(sc.gameKeepalive(ctx))
		}
	}
}

// streamStarted is one stream_start transition. Desktop streams
// (Sunshine) are a pure no-op beyond the gauge and the event — the
// brain stays up, the workers keep their leases. Game streams take the
// full GPU path: hold, revoke, brain out.
func (sc *streamController) streamStarted(ctx context.Context, stream streamwatcher.StreamType) {
	sc.active.Set(1)
	if !stream.IsGame() {
		sc.log.Info("desktop stream started — the brain and the GPU leases stay up", "stream", stream.String())
		sc.svc.emit(ctx, queue.StreamEvent("started", map[string]any{
			"stream": stream.String(),
			"game":   false,
		}))
		return
	}
	sc.log.Info("game stream started — holding the GPU and stopping the brain", "stream", stream.String())
	// 1. Hold first: a worker re-asking within this tick must queue,
	//    not be granted into the headroom the game is about to take
	//    (its VRAM lands in the ledger only at the next measured
	//    refresh, up to REFRESH_INTERVAL away).
	sc.svc.ledger.SetStreamActive(true)
	// 2. Revoke the capability workers' GPU leases — granted AND
	//    queued (the plan says "all GPU leases"; the queued ones'
	//    workers re-ask on their poll tick and sit in the held queue
	//    again, which is the same state with a fresh position). The
	//    brain's standing lease is exempt by construction.
	revoked := sc.svc.ledger.RevokeGPULeases()
	for _, lease := range revoked {
		sc.svc.emit(ctx, queue.LeaseEvent("released", map[string]any{
			"lease":  lease,
			"reason": "game stream " + stream.String() + " active",
		}))
	}
	// 3. Brain out: the game wants the VRAM, so the window opens for
	//    the game. The game-hold marker goes down BEFORE the brain
	//    stop: a round that gets cut (step 4) is back in the ready
	//    queue in a second, and the marker is what keeps
	//    `jarvis-jobs next` from starting a new local round into the
	//    swap. The stop fails while a round runs (invariant 1's guard
	//    — the round gets cut, step 4) and while the brain is already
	//    out for the game (steady state) — the keepalive owns both
	//    retries.
	sc.mu.Lock()
	sc.gameActive = true
	sc.mu.Unlock()
	sc.setGameHold()
	if sc.svc.window != nil {
		if err := sc.acquireForGame(ctx, "steam game active ("+stream.String()+")"); err != nil {
			sc.log.Error("brain-out for the game not done — the keepalive retries every 5 s while the brain is up", "err", err)
		}
	}
	sc.svc.emit(ctx, queue.StreamEvent("started", map[string]any{
		"stream":         stream.String(),
		"game":           true,
		"revoked_leases": len(revoked),
	}))
}

// streamStopped is one stream_end transition. For a game stream the
// window closes FIRST (only if it is the game's — ReleaseFromGame
// blocks until the brain is back), and only then the GPU hold and the
// game-hold marker come off: a new local round must not start while the
// brain is still down. If the close fails, the hold and the marker stay
// on and a sweeper clears them once the brain is back. The queued GPU
// leases that now fit grant (the game's VRAM is gone; the last measured
// refresh still counts it, so the grants land on a conservative
// remainder and the next refresh corrects).
func (sc *streamController) streamStopped(ctx context.Context) {
	sc.active.Set(0)
	sc.preemptNotified = false
	sc.mu.Lock()
	wasGame := sc.gameActive
	sc.gameActive = false
	sc.mu.Unlock()
	if wasGame {
		sc.log.Info("game stream ended — bringing the brain back and releasing the GPU hold")
		brainBack := true
		if sc.svc.window != nil {
			if err := sc.svc.window.ReleaseFromGame(ctx, "steam game ended"); err != nil {
				sc.log.Error("window close after the game failed — the GPU hold and the game-hold marker stay on until the brain is back", "err", err)
				brainBack = false
			}
		}
		if brainBack {
			sc.releaseGameHold(ctx)
		} else {
			go sc.sweepBrainBack(ctx)
		}
	}
	for _, out := range sc.svc.ledger.Reevaluate() {
		sc.svc.apply(ctx, out)
	}
	sc.svc.emit(ctx, queue.StreamEvent("stopped", map[string]any{"game": wasGame}))
}

// gameKeepalive runs while a game is active (Run's timer, at the
// interval this returns: 5 s while the brain is still up or the window
// is not the game's yet, 60 s in the steady state). Two jobs, both
// self-healing:
//
//   - the brain-out: a round running when the game started made
//     jarvis-swap abort on its guard; the round was cut at stream
//     start, so re-acquire — the retry succeeds on the next 5 s tick.
//   - the maintenance window: jarvis-swap out arms a 4 h maintenance
//     deadline that keeps jarvis-watchdog from restarting the brain;
//     a session longer than that would re-arm the brain under the
//     game, so re-arm it while it is still active.
//
// A closed marker with the brain down is NOT the game's problem: that
// is the watchdog's (brain restarted, or a manual swap in) — the
// watchdog owns that state, the game just logs it.
func (sc *streamController) gameKeepalive(ctx context.Context) time.Duration {
	if !sc.gameActive || sc.svc.window == nil {
		return 60 * time.Second
	}
	open, forCaps, _, err := sc.svc.window.swap.State()
	if err != nil {
		sc.log.Warn("game keepalive: cannot read the swap state", "err", err)
		return 5 * time.Second
	}
	brainUp := sc.probeBrain(ctx)
	switch {
	case !open && !brainUp:
		sc.log.Info("game keepalive: brain down without a window marker — the watchdog owns this, not the game")
		return 60 * time.Second
	case !open && brainUp:
		if err := sc.acquireForGame(ctx, "steam game active (keepalive)"); err != nil {
			sc.log.Error("game keepalive: brain-out failed", "err", err)
		}
		return 5 * time.Second
	case open && forContainsGame(forCaps) && brainUp:
		sc.log.Warn("game keepalive: the brain is up under an open game window (re-armed mid-game?) — re-acquiring")
		if err := sc.acquireForGame(ctx, "steam game active (keepalive)"); err != nil {
			sc.log.Error("game keepalive: brain-out failed", "err", err)
		}
		return 5 * time.Second
	case open && forContainsGame(forCaps):
		if !sc.maintainAlive() {
			if err := sc.maintainRearm(); err != nil {
				sc.log.Error("game keepalive: maintenance re-arm failed", "err", err)
			} else {
				sc.log.Info("game keepalive: watchdog maintenance window re-armed (4h)")
			}
		}
		return 60 * time.Second
	case open:
		// A photo window is open (queue-triggered, or it opened while
		// the game ran and ReleaseFromGame left it). The game wants it
		// for itself — convert it (the script stops the art service).
		sc.log.Info("game keepalive: an open window is not the game's — converting it to the game window", "for", forCaps)
		if err := sc.acquireForGame(ctx, "steam game active (keepalive)"); err != nil {
			sc.log.Error("game keepalive: brain-out failed", "err", err)
		}
		return 5 * time.Second
	}
	return 60 * time.Second
}

// localRoundsRunning lists the driver-local rounds in flight right now
// (the same question jarvis-swap's round guard asks: `jarvis-jobs list
// running`, then each one's driver). A round whose driver is unreadable
// or empty is counted as local — that mirrors the guard's own rule, and
// a local round is the only kind the guard protects.
func localRoundsRunning() []string {
	out, err := exec.Command("jarvis-jobs", "list", "running").Output()
	if err != nil {
		return nil
	}
	var rounds []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "running" {
			continue
		}
		id := f[1]
		driver := ""
		if show, err := exec.Command("jarvis-jobs", "show", id).Output(); err == nil {
			for _, l := range strings.Split(string(show), "\n") {
				if d, ok := strings.CutPrefix(strings.TrimSpace(l), "driver: "); ok {
					driver = strings.TrimSpace(d)
					break
				}
			}
		}
		if driver == "" || driver == "local" {
			rounds = append(rounds, id)
		}
	}
	return rounds
}

// acquireForGame is AcquireForGame plus the preemption: when the
// brain-out fails and driver-local rounds are in flight, they are cut —
// the same restart-signal the !photonow preemption writes (the runner's
// signalWatcher kills the round within a second and the job goes back
// to ready, for a fresh run) — and the owner gets the one Discord line.
// A failure with no local round in flight is returned unchanged (it is
// the steady state or something the keepalive owns).
func (sc *streamController) acquireForGame(ctx context.Context, reason string) error {
	err := sc.svc.window.AcquireForGame(ctx, reason)
	if err == nil {
		return nil
	}
	rounds := sc.localRounds()
	if len(rounds) == 0 {
		return err
	}
	if werr := sc.writeRestartSignal(); werr != nil {
		sc.log.Error("game: writing the restart signal failed — the round guard will keep aborting the brain-out", "err", werr)
	}
	sc.log.Info("game: cutting the running local round(s) to free the 4090 for the game", "rounds", rounds)
	if !sc.preemptNotified {
		sc.preemptNotified = true
		msg := "A game just started — I cut the running round " + strings.Join(rounds, ", ") +
			" to free the 4090 for it. The round re-runs in a fresh conversation after the game ends."
		if nerr := sc.notifyOwner(msg); nerr != nil {
			sc.log.Warn("game: could not tell the owner about the cut", "err", nerr)
		}
	}
	return err
}

// writeRestartSignal drops the agent-run restart-signal file the runner
// polls every second (internal/agentjob/run.go's signalWatcher kills the
// round and requeues the job on it). The content is "steam-game" —
// anything but the "!fixnow" sentinel re-runs the job in a fresh
// conversation, which is what a cut round wants (its context is stale
// mid-game anyway).
func (sc *streamController) writeRestartSignal() error {
	return os.WriteFile(filepath.Join(sc.signalDir, "restart-signal"), []byte("steam-game"), 0o644)
}

// setGameHold marks that a game wants the 4090: `jarvis-jobs next`
// skips driver-local jobs while the marker exists (cmd/jarvis-jobs),
// so no new local round can start into the brain-out. Set BEFORE the
// brain stop (the cut round is requeued in a second), cleared once the
// brain is back.
func (sc *streamController) setGameHold() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.gameHold == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(sc.gameHold), 0o755); err != nil {
		sc.log.Warn("game-hold marker: cannot create its directory", "err", err)
		return
	}
	if err := os.WriteFile(sc.gameHold, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		sc.log.Warn("game-hold marker: could not write it", "err", err)
	}
}

// clearGameHold removes the game-hold marker. No-op while a game is
// still active (the marker belongs to the current session then) and
// when the path is unset.
func (sc *streamController) clearGameHold() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.gameActive || sc.gameHold == "" {
		return
	}
	if err := os.Remove(sc.gameHold); err != nil && !errors.Is(err, os.ErrNotExist) {
		sc.log.Warn("game-hold marker: could not remove it", "err", err)
	}
}

// gameHoldActive reports whether the marker exists (resourced started
// mid-game, or a close failed and the sweeper has not landed yet).
func (sc *streamController) gameHoldActive() bool {
	if sc.gameHold == "" {
		return false
	}
	_, err := os.Stat(sc.gameHold)
	return err == nil
}

// releaseGameHold takes the GPU hold off and clears the game-hold
// marker — called only once the brain is back.
func (sc *streamController) releaseGameHold(ctx context.Context) {
	sc.svc.ledger.SetStreamActive(false)
	sc.clearGameHold()
}

// sweepBrainBack clears the GPU hold and the game-hold marker once the
// brain is back. It exists for two cases: the stream_end close failed
// (the brain is still out, so the hold and the marker stay on), and a
// resourced that started with the marker left behind (a restart mid-
// game, or a missed end transition) — the marker must never park local
// rounds forever. While a NEW game is active it backs off: that game
// owns the GPU and owns clearing the hold.
func (sc *streamController) sweepBrainBack(ctx context.Context) {
	tick := time.NewTicker(sc.sweepEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !sc.probeBrain(ctx) {
				continue
			}
			sc.mu.Lock()
			if sc.gameActive { // a new game owns the GPU now
				sc.mu.Unlock()
				return
			}
			sc.svc.ledger.SetStreamActive(false)
			sc.mu.Unlock()
			sc.clearGameHold()
			for _, out := range sc.svc.ledger.Reevaluate() {
				sc.svc.apply(ctx, out)
			}
			sc.log.Info("game: the brain is back — GPU hold and game-hold marker cleared")
			return
		}
	}
}

// splitCSV splits a comma-separated env value into trimmed, non-empty
// parts ("", "a", "b" → ["a","b"]).
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
