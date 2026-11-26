// loopd is the event-loop agent runtime daemon (ELR-6 skeleton: it only
// serves /healthz; the scheduler arrives on later cards).
package main

import (
	"log"
	"net/http"
	"os"
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
	log.Printf("loopd listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
