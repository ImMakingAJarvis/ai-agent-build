// Dashboard: web UI for monitoring the JARVIS job queue.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/dashboard"
)

func main() {
	srv := dashboard.New()

	// Graceful shutdown on SIGINT/SIGTERM.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("dashboard: shutting down")
		os.Exit(0)
	}()

	if err := srv.Serve(); err != nil {
		log.Fatalf("dashboard: %v", err)
	}
}
