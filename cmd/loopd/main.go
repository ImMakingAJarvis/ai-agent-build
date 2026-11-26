// loopd is the event-loop agent runtime daemon. The ELR-6 skeleton
// served only /healthz; card ELR-33 adds /metrics, the loop's own
// private registry (the scheduler of a later card builds the loop with
// the same *Metrics and records into it).
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/lock"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/loop"
)

func main() {
	addr := os.Getenv("LOOPD_ADDR")
	if addr == "" {
		addr = ":9110"
	}
	// One runtime at a time (card ELR-87): the lock lives in the job
	// store directory, the one directory loopd in its container and
	// loopctl on the host both see. The kernel releases the flock when
	// this process dies, so a crash never leaves a stale lock.
	lockPath := filepath.Join(agentjob.DataDir(), "loop.lock")
	lk, err := lock.Acquire(lockPath)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			fmt.Fprintf(os.Stderr, "loopd: another runtime holds %s: %s\n", lockPath, lock.Holder(lockPath))
			os.Exit(3)
		}
		log.Fatalf("loopd: lock %s: %v", lockPath, err)
	}
	defer lk.Release()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			log.Printf("loopd: write healthz: %v", err)
		}
	})
	// The loop's private registry, served the way resourced serves its
	// own: a loopd restart resets its series, nothing else's.
	_, reg := loop.NewMetrics()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	log.Printf("loopd listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
