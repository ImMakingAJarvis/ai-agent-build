// switchboard is the service half of the event-driven brain's wait/wake loop
// (plan task 14, docs/event-driven.md). It owns listeners — durable "when a
// message lands on queue Q, do action A" records — and, in a single poll loop,
// peeks every listened queue and:
//
//   - wake:    peeks a result (leaves it for the woken round), records it
//     against the job, and wakes the job back to ready;
//   - forward: republishes the message to a target queue (acked after the
//     broker confirms the copy);
//   - notify:  sends a Discord notification and acks the message.
//
// A listener whose deadline passes with its correlation unarrived wakes the
// job with status=timeout. Listeners live as one JSON file each under
// ~/.local/share/jarvis/listeners/ (the agentjob data root), so they survive a
// switchboard restart; the poll loop re-reads them every pass and re-opens the
// broker connection after a broker restart.
//
// Configuration (env):
//
//	RABBITMQ_URL        the broker. Default amqp://jarvis:jarvis-local@localhost:5672.
//	JARVIS_DATA_DIR     the agentjob data root. Default ~/.local/share/jarvis.
//	LISTENERS_DIR       the listener store dir. Default <JARVIS_DATA_DIR>/listeners.
//	LISTEN_ADDR         Prometheus scrape address. Default :9103.
//	POLL_INTERVAL       how often the listener queues are peeked. Default 250ms.
//	TIMEOUT_INTERVAL    how often waiting-job deadlines are checked. Default 5s.
//	MAX_BATCH           max messages fetched per queue per pass. Default 100.
//
// Discord notify uses the standard secrets file (internal/config); with no
// bot configured, notify actions are logged and acked rather than sent.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/boardnote"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/chat"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/config"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/switchboard"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key, def string) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		d, _ := time.ParseDuration(def)
		return d
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Fatalf("switchboard: bad %s %q", key, v)
	}
	return d
}

func envInt(key, def string) int {
	v := os.Getenv(key)
	if v == "" {
		n, _ := strconv.Atoi(def)
		return n
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Fatalf("switchboard: bad %s %q", key, v)
	}
	return n
}

// connectWithRetry dials the broker, backing off until it is reachable (the
// compose stack may bring rabbitmq up a little after the switchboard) or ctx
// is cancelled.
func connectWithRetry(ctx context.Context) (*queue.Conn, error) {
	backoff := time.Second
	for {
		conn, err := queue.NewConn()
		if err == nil {
			return conn, nil
		}
		log.Printf("switchboard: broker not ready: %v", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// buildNotifier returns a Discord notify function, or nil when no bot is
// configured. The Discord client is built lazily on first use so a missing
// token or a Discord outage at startup does not stop the switchboard.
func buildNotifier() func(context.Context, string) error {
	cfg, err := config.Load()
	if err != nil {
		log.Printf("switchboard: config: %v (notify disabled)", err)
		return nil
	}
	p := cfg.Primary()
	if p == nil || p.Token == "" || len(p.AllowedUsers) == 0 || p.AllowedUsers[0] == "" {
		log.Printf("switchboard: no Discord bot configured (notify actions are logged, not sent)")
		return nil
	}
	var (
		mu sync.Mutex
		d  *chat.Discord
	)
	return func(ctx context.Context, text string) error {
		mu.Lock()
		if d == nil {
			nd, err := chat.NewDiscord(chat.Config{Token: p.Token, OwnerID: p.AllowedUsers[0]})
			if err != nil {
				mu.Unlock()
				return fmt.Errorf("discord: %w", err)
			}
			d = nd
		}
		mu.Unlock()
		return d.Notify(ctx, text)
	}
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("switchboard: ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dataDir := envOr("JARVIS_DATA_DIR", agentjob.DataDir())
	listenersDir := envOr("LISTENERS_DIR", filepath.Join(dataDir, "listeners"))
	listen := envOr("LISTEN_ADDR", ":9103")

	jobs, err := agentjob.Open(dataDir)
	if err != nil {
		log.Fatalf("switchboard: open job store: %v", err)
	}
	listenStore, err := switchboard.OpenStore(listenersDir)
	if err != nil {
		log.Fatalf("switchboard: open listener store: %v", err)
	}
	metrics, reg := switchboard.NewMetrics()

	// Metrics endpoint (private registry — a restart never disturbs another
	// service's series).
	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	srv := &http.Server{Addr: listen}
	go func() {
		log.Printf("switchboard: metrics on %s", listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("switchboard: metrics http: %v", err)
		}
	}()
	defer srv.Shutdown(context.Background())

	conn, err := connectWithRetry(ctx)
	if err != nil {
		log.Fatalf("switchboard: connect to broker: %v", err)
	}
	defer conn.Close()

	// The board-mapped job's "resumed" Plane comment (plan §3.5, invariant
	// 10): the switchboard wakes the job, this hook posts the card comment.
	// Best-effort — boardnote returns nil when the title carries no Plane
	// ref or Plane is not configured, and logs (never fails) otherwise.
	onWake := func(j agentjob.Job, rec agentjob.WakeRecord) {
		missing := 0
		if miss, err := jobs.MissingResults(j.ID); err == nil {
			missing = len(miss)
		}
		// The post is off the poll loop: a hung Plane must not delay the
		// next wake (the switchboard is single-goroutine; the store's
		// atomic-rename writes make the async read safe).
		go boardnote.PostWake(j, rec, missing)
	}

	sb := switchboard.New(switchboard.Config{
		Conn:            conn,
		Jobs:            jobs,
		Listen:          listenStore,
		M:               metrics,
		Notify:          buildNotifier(),
		OnWake:          onWake,
		PollInterval:    envDuration("POLL_INTERVAL", "250ms"),
		TimeoutInterval: envDuration("TIMEOUT_INTERVAL", "5s"),
		MaxBatch:        envInt("MAX_BATCH", "100"),
	})
	log.Printf("switchboard: listeners in %s, jobs in %s", listenersDir, dataDir)

	if err := sb.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("switchboard: run: %v", err)
	}
}
