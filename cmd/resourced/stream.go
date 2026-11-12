// The game/stream watchdog (plan §3.6 rule 2, task EVB-29): "the game
// watchdog wins — when a stream is active, revoke all GPU leases
// (capability workers drain / unload)". This is the replacement for the
// old direct GPU-queue pause as far as capability workers are concerned:
// before EVB-29 a stream stopped the brain engine (worker side) and
// paused the brain jobs' GPU queue (task-runner side), but the
// capability workers — which hold the 4090 through their leases — had no
// stream awareness at all.
//
// resourced runs its own streamwatcher (the same package and the same
// detection as the existing host watchdogs: Sunshine over /proc or its
// API, Steam Remote Play over /proc, NVENC encoder utilization via
// nvidia-smi as the fallback that always works from a host service).
// On a stream start:
//
//  1. the ledger's GPU hold turns on (SetStreamActive) — no new GPU
//     lease may be granted while the stream's own VRAM is not in the
//     ledger's remainder yet;
//  2. every capability worker's GPU lease is revoked (RevokeGPULeases)
//     with a lease.released event per lease (invariant 10). The brain's
//     standing lease is exempt — rules 1 and 3: a working brain is
//     never evicted, and the brain comes back through jarvis-swap, not
//     by the ledger shrinking it;
//  3. an open swap window closes (CloseForGame): the brain comes back
//     BEFORE the game gets its VRAM, same order as a drained close.
//
// The revoked cap workers notice on their next /state poll (LeasePollS,
// default 30 s) and run the task-26 drain: finish the in-flight
// delivery, cancel the consumer, take no new work — no half-written
// artifacts, no lost messages (invariant 9). Their models stay loaded
// until the idle-unload (IdleS), so the game's VRAM lands within a
// minute or two of the last in-flight job; the art service's own
// min_free gate (ART_MIN_FREE_MB, 12 GB) is the defense in depth that
// keeps a new generation from starting into the game's headroom.
//
// CPU and iGPU leases are never touched — the rule is GPU-only (a game
// stream does not compete for the box's RAM).
//
// On a stream end the hold clears and Reevaluate grants the queued GPU
// leases that now fit; the workers re-ask for a lease on their poll
// tick and resume.
//
// The brain side of the old pause is untouched: the worker container's
// streamwatcher still stops/starts the engine, and the task-runner's
// still pauses the brain jobs' GPU queue (jobs.gpu). This controller is
// only the capability-worker half — the part the lease ledger owns.
//
// Config (env, all optional — the NVENC fallback needs none of them):
//
//	SUNSHINE_ENDPOINT  Sunshine API base (e.g. http://127.0.0.1:8080/api/v2).
//	STEAM_ENDPOINT     Steam in-home streaming status page.

package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/streamwatcher"
)

// streamController owns the watchdog: the watcher's transitions and the
// ledger/window actions they trigger. It runs on its own goroutine (the
// window close blocks for the model load, minutes) and never blocks the
// ledger's refresh loop.
type streamController struct {
	svc *service
	log *slog.Logger

	active prometheus.Gauge // resourced_stream_active
}

func newStreamController(svc *service, reg *prometheus.Registry, log *slog.Logger) *streamController {
	sc := &streamController{svc: svc, log: log}
	sc.active = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "resourced_stream_active",
		Help: "1 while a game stream (Sunshine/Steam) is active: GPU leases revoked, GPU queue held.",
	})
	reg.MustRegister(sc.active)
	return sc
}

// Run starts the watcher and processes its state transitions until ctx
// is done. Start emits the initial state as well, so a resourced that
// boots mid-stream takes the game path immediately (the watcher's first
// StateChange is the current state, not just later transitions).
func (sc *streamController) Run(ctx context.Context) {
	watch := streamwatcher.New(streamwatcher.Config{
		SunshineEndpoint: os.Getenv("SUNSHINE_ENDPOINT"),
		SteamEndpoint:    os.Getenv("STEAM_ENDPOINT"),
	})
	ch := make(chan streamwatcher.StateChange, 1)
	watch.Start(ch)
	sc.log.Info("stream watchdog started (game → revoke GPU leases, close the window)")
	for {
		select {
		case <-ctx.Done():
			return
		case change := <-ch:
			if change.Stream != streamwatcher.NoStream {
				sc.streamStarted(ctx, change.Stream.String())
			} else {
				sc.streamStopped(ctx)
			}
		}
	}
}

// streamStarted is one stream_start transition: hold the GPU, revoke
// the capability workers' GPU leases, close an open window.
func (sc *streamController) streamStarted(ctx context.Context, stream string) {
	sc.log.Info("game stream started — revoking the GPU leases", "stream", stream)
	sc.active.Set(1)
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
			"reason": "game stream " + stream + " active",
		}))
	}
	// 3. Close an open swap window: the brain comes back before the
	//    game gets its VRAM. Nil-safe (no catalog → no controller).
	if sc.svc.window != nil {
		if err := sc.svc.window.CloseForGame(ctx, "game stream "+stream+" active"); err != nil {
			sc.log.Error("window close for the game failed — the next tick will close it", "err", err)
		}
	}
	sc.svc.emit(ctx, queue.StreamEvent("started", map[string]any{
		"stream":         stream,
		"revoked_leases": len(revoked),
	}))
}

// streamStopped is one stream_end transition: clear the hold and grant
// the queued GPU leases that now fit (the stream's VRAM is gone; the
// last measured refresh still counts it, so the grants land on a
// conservative remainder and the next refresh corrects).
func (sc *streamController) streamStopped(ctx context.Context) {
	sc.log.Info("game stream ended — releasing the GPU hold")
	sc.active.Set(0)
	sc.svc.ledger.SetStreamActive(false)
	for _, out := range sc.svc.ledger.Reevaluate() {
		sc.svc.apply(ctx, out)
	}
	sc.svc.emit(ctx, queue.StreamEvent("stopped", nil))
}
