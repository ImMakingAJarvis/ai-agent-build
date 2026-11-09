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

	// Persistent RabbitMQ connection for queue peeks and dead letters
	// (EVB-10). A down broker must not kill the dashboard — the affected
	// sections just show an error until it is back.
	if err := srv.StartRabbitMQ(); err != nil {
		log.Printf("dashboard: rabbitmq: %v (queue peeks and dead letters disabled)", err)
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("dashboard: shutting down")
		srv.CloseRabbitMQ()
		os.Exit(0)
	}()

	if err := srv.Serve(); err != nil {
		log.Fatalf("dashboard: %v", err)
	}
}
