package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/capworker"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func main() {
	configPath := flag.String("config", "capworker-remote.yaml", "path to the node config")
	flag.Parse()

	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("capworker-remote: ")

	cfg, err := Load(*configPath)
	if err != nil {
		// A config error is an operator error, not a broker error:
		// exit 2 (distinct from a runtime failure) and do not retry.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("capworker-remote: node remote:%s serving %d capability(ies)", cfg.Node, len(cfg.Capabilities))

	opts, err := cfg.Broker.dialOpts()
	if err != nil {
		log.Fatalf("capworker-remote: %v", err)
	}
	conn, err := connectWithRetry(ctx, opts)
	if err != nil {
		log.Fatalf("capworker-remote: connect to broker: %v", err)
	}
	defer conn.Close()

	// Events: the worker.<node>.* heartbeats and job.<cap>.* lifecycle
	// events. The node id is the source, so the box sees one heartbeat
	// stream per node (each event carries its capability).
	events, err := queue.NewEventPublisher(conn, cfg.Node)
	if err != nil {
		log.Printf("capworker-remote: event publisher: %v (events disabled)", err)
		events = nil
	}
	if events != nil {
		defer events.Close()
	}

	var wg sync.WaitGroup
	if cfg.Resourced.URL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSelfReport(ctx, cfg.Node, cfg.Resourced.URL, time.Duration(cfg.SelfReport), log.Default())
		}()
	}

	runErrs := make(chan error, len(cfg.Capabilities))
	for _, capCfg := range cfg.Capabilities {
		h, model := handlerFor(capCfg, cfg.Node)
		w := capworker.New(capworker.Config{
			Cap:        capCfg.Name,
			Name:       cfg.Node, // worker.<node>.* heartbeats; Results carry the node
			Model:      model,
			Location:   "remote:" + cfg.Node, // consumer priority 1 (LocationPriority)
			Conn:       conn,
			Events:     events,
			HeartbeatS: time.Duration(cfg.Heartbeat),
			OutDirBase: cfg.Artifacts.Dir,
			Handler:    h,
			Log:        log.Default(),
		})
		wg.Add(1)
		go func(w *capworker.Worker, name string) {
			defer wg.Done()
			log.Printf("capworker-remote: cap %s: consuming %s", name, w.QueueName())
			if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				runErrs <- fmt.Errorf("cap %s: %w", name, err)
			}
		}(w, capCfg.Name)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		<-done // let the workers and the self-report drain
		log.Printf("capworker-remote: stopped")
	case err := <-runErrs:
		log.Fatalf("capworker-remote: %v", err)
	}
}

// handlerFor builds a capability's handler from its config.
func handlerFor(cap Capability, node string) (capworker.Handler, string) {
	if cap.Builtin == "echo" {
		return echoHandler{delay: time.Duration(cap.Delay), node: node}, "echo"
	}
	return commandHandler{
		command: cap.Command,
		env:     cap.Env,
		timeout: time.Duration(cap.Timeout),
	}, "command"
}

// connectWithRetry dials the broker, backing off until it is reachable
// (the broker may come up a little after the node's service starts).
// Only the URL's host is logged — never the credentials.
func connectWithRetry(ctx context.Context, opts queue.DialOpts) (*queue.Conn, error) {
	backoff := time.Second
	for {
		conn, err := queue.NewConnOpts(opts)
		if err == nil {
			return conn, nil
		}
		log.Printf("capworker-remote: broker not ready (%s): %v", logURL(opts.URL), err)
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

// logURL strips the userinfo from a broker URL for logging.
func logURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable broker url>"
	}
	u.User = nil
	return u.String()
}
