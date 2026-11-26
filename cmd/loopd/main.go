// loopd is the event-loop agent runtime daemon. The ELR-6 skeleton
// served only /healthz; card ELR-33 adds /metrics, the loop's own
// private registry (the scheduler of a later card builds the loop with
// the same *Metrics and records into it).
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ImMakingAJarvis/ai-agent-build/runtime/loop"
)

func main() {
	addr := os.Getenv("LOOPD_ADDR")
	if addr == "" {
		addr = ":9110"
	}
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
