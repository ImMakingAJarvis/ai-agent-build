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
// owns the jarvis-swap call and a 60 s game keepalive:
//
//   - while a round runs, jarvis-swap's round guard aborts the brain
//     stop (invariant 1) — the keepalive retries every 60 s until the
//     round is done, so the brain goes out as soon as it legally may;
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
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

	gameActive bool // a game-kind stream is running (hold on, keepalive armed)

	// Test-overridable probes: the brain liveness check and the
	// watchdog-maintenance status/re-arm (window.go owns the defaults).
	probeBrain    func(ctx context.Context) bool
	maintainAlive func() bool
	maintainRearm func() error
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
	reg.MustRegister(sc.active)
	return sc
}

// Run starts the watcher and processes its state transitions until ctx
// is done. Start emits the initial state as well, so a resourced that
// boots mid-stream takes the game path immediately (the watcher's first
// StateChange is the current state, not just later transitions). The
// 60 s ticker drives the game keepalive (brain-out retries, maintenance
// re-arm) — the stream transitions are event-driven, this is the only
// periodic part.
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
	keepalive := time.NewTicker(60 * time.Second)
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
			sc.gameKeepalive(ctx)
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
	//    the game. It fails while a round runs (invariant 1's guard)
	//    and while the brain is already out for the game (steady
	//    state) — the keepalive owns both retries.
	sc.gameActive = true
	if sc.svc.window != nil {
		if err := sc.svc.window.AcquireForGame(ctx, "steam game active ("+stream.String()+")"); err != nil {
			sc.log.Error("brain-out for the game not done — the keepalive retries every 60 s", "err", err)
		}
	}
	sc.svc.emit(ctx, queue.StreamEvent("started", map[string]any{
		"stream":         stream.String(),
		"game":           true,
		"revoked_leases": len(revoked),
	}))
}

// streamStopped is one stream_end transition: clear the hold and grant
// the queued GPU leases that now fit (the game's VRAM is gone; the
// last measured refresh still counts it, so the grants land on a
// conservative remainder and the next refresh corrects). For a game
// stream the window closes first — only if it is the game's.
func (sc *streamController) streamStopped(ctx context.Context) {
	sc.active.Set(0)
	wasGame := sc.gameActive
	sc.gameActive = false
	if wasGame {
		sc.log.Info("game stream ended — bringing the brain back and releasing the GPU hold")
		sc.svc.ledger.SetStreamActive(false)
		if sc.svc.window != nil {
			if err := sc.svc.window.ReleaseFromGame(ctx, "steam game ended"); err != nil {
				sc.log.Error("window close after the game failed — the next tick will handle it", "err", err)
			}
		}
	}
	for _, out := range sc.svc.ledger.Reevaluate() {
		sc.svc.apply(ctx, out)
	}
	sc.svc.emit(ctx, queue.StreamEvent("stopped", map[string]any{"game": wasGame}))
}

// gameKeepalive runs once a minute while a game is active. Two
// jobs, both self-healing:
//
//   - the brain-out: a round running when the game started (or that
//     started during the game) made jarvis-swap abort on its guard;
//     re-acquire so the brain goes out the moment the round is done.
//   - the maintenance window: jarvis-swap out arms a 4 h maintenance
//     deadline that keeps jarvis-watchdog from restarting the brain;
//     a session longer than that would re-arm the brain under the
//     game, so re-arm it while it is still active.
//
// A closed marker with the brain down is NOT the game's problem: that
// is the watchdog's (brain restarted, or a manual swap in) — the
// watchdog owns that state, the game just logs it.
func (sc *streamController) gameKeepalive(ctx context.Context) {
	if !sc.gameActive || sc.svc.window == nil {
		return
	}
	open, forCaps, _, err := sc.svc.window.swap.State()
	if err != nil {
		sc.log.Warn("game keepalive: cannot read the swap state", "err", err)
		return
	}
	brainUp := sc.probeBrain(ctx)
	switch {
	case !open && !brainUp:
		sc.log.Info("game keepalive: brain down without a window marker — the watchdog owns this, not the game")
	case !open && brainUp:
		if err := sc.svc.window.AcquireForGame(ctx, "steam game active (keepalive)"); err != nil {
			sc.log.Error("game keepalive: brain-out failed", "err", err)
		}
	case open && forContainsGame(forCaps) && brainUp:
		sc.log.Warn("game keepalive: the brain is up under an open game window (re-armed mid-game?) — re-acquiring")
		if err := sc.svc.window.AcquireForGame(ctx, "steam game active (keepalive)"); err != nil {
			sc.log.Error("game keepalive: brain-out failed", "err", err)
		}
	case open && forContainsGame(forCaps):
		if !sc.maintainAlive() {
			if err := sc.maintainRearm(); err != nil {
				sc.log.Error("game keepalive: maintenance re-arm failed", "err", err)
			} else {
				sc.log.Info("game keepalive: watchdog maintenance window re-armed (4h)")
			}
		}
	case open:
		// A photo window is open (queue-triggered, or it opened while
		// the game ran and ReleaseFromGame left it). The game wants it
		// for itself — convert it (the script stops the art service).
		sc.log.Info("game keepalive: an open window is not the game's — converting it to the game window", "for", forCaps)
		if err := sc.svc.window.AcquireForGame(ctx, "steam game active (keepalive)"); err != nil {
			sc.log.Error("game keepalive: brain-out failed", "err", err)
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
