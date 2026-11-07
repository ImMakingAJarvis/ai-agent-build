// token-ledger is the persistent token counter for the Grafana dashboard.
//
// It polls the llama.cpp engines' /metrics endpoints, folds their
// (per-run, reset-on-restart) llamacpp:* token counters into an
// all-time cumulative kept in a JSON state file, and re-exposes the
// cumulatives as a never-resetting Prometheus counter on :9102. Grafana
// reads the all-time total from it and derives per-day/week/month buckets
// with increase(jarvis_tokens_total[1d|1w|1M]).
//
// Configuration (env):
//
//	ENGINES        comma-separated name=url pairs (url = engine base,
//	               /metrics is appended). Default:
//	               brain=http://127.0.0.1:11434,igpu=http://127.0.0.1:11435
//	LISTEN_ADDR    Prometheus scrape address. Default :9102.
//	STATE_DIR      where state.json lives. Default
//	               $HOME/.local/share/jarvis/token-ledger.
//	POLL_INTERVAL  how often to poll the engines. Default 15s (matches
//	               the Prometheus scrape interval).
package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/tokenledger"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type engine struct {
	name string
	url  string // base URL; /metrics is appended
}

func parseEngines(spec string) []engine {
	var out []engine
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, url, ok := strings.Cut(part, "=")
		if !ok || name == "" || url == "" {
			log.Fatalf("token-ledger: bad ENGINES entry %q (want name=url)", part)
		}
		out = append(out, engine{name: name, url: strings.TrimSuffix(url, "/")})
	}
	if len(out) == 0 {
		log.Fatal("token-ledger: ENGINES is empty")
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	engines := parseEngines(envOr("ENGINES", "brain=http://127.0.0.1:11434,igpu=http://127.0.0.1:11435"))
	listen := envOr("LISTEN_ADDR", ":9102")
	poll, err := time.ParseDuration(envOr("POLL_INTERVAL", "15s"))
	if err != nil || poll <= 0 {
		log.Fatalf("token-ledger: bad POLL_INTERVAL %q", os.Getenv("POLL_INTERVAL"))
	}
	stateDir := envOr("STATE_DIR", filepath.Join(os.Getenv("HOME"), ".local", "share", "jarvis", "token-ledger"))
	statePath := filepath.Join(stateDir, "state.json")

	ledger, err := tokenledger.New(statePath)
	if err != nil {
		log.Fatalf("token-ledger: load state: %v", err)
	}
	log.Printf("token-ledger: state %s, engines %v, poll %s", statePath, engines, poll)

	// Never-resetting cumulative counters: one series per engine + kind.
	tokensTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "jarvis_tokens_total",
			Help: "All-time tokens processed per engine, accumulated across engine restarts by the token ledger.",
		},
		[]string{"engine", "kind"},
	)
	engineUp := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "jarvis_token_ledger_engine_up",
			Help: "1 if the engine's /metrics was reachable on the last poll.",
		},
		[]string{"engine"},
	)
	lastPoll := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "jarvis_token_ledger_last_poll_timestamp_seconds",
			Help: "Unix time of the ledger's last successful poll pass.",
		},
	)
	reg := prometheus.NewRegistry()
	reg.MustRegister(tokensTotal, engineUp, lastPoll)

	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	srv := &http.Server{Addr: listen}
	go func() {
		log.Printf("token-ledger: listening on %s", listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("token-ledger: http: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	pollOnce := func() {
		for _, e := range engines {
			body, ok := fetch(client, e.url+"/metrics")
			engineUp.WithLabelValues(e.name).Set(boolToFloat(ok))
			if !ok {
				log.Printf("token-ledger: %s: /metrics unreachable, keeping state", e.name)
				continue
			}
			values, err := tokenledger.ParseCounters(body)
			if err != nil {
				log.Printf("token-ledger: %s: parse: %v", e.name, err)
				continue
			}
			if len(values) == 0 {
				log.Printf("token-ledger: %s: no llamacpp token counters in /metrics (is --metrics on?)", e.name)
				continue
			}
			ledger.Observe(e.name, values)
		}
		if err := ledger.Save(); err != nil {
			log.Printf("token-ledger: save state: %v", err)
		}
		lastPoll.SetToCurrentTime()
	}

	pollOnce()
	for {
		select {
		case <-ctx.Done():
			if err := ledger.Save(); err != nil {
				log.Printf("token-ledger: final save: %v", err)
			}
			return
		case <-ticker.C:
			pollOnce()
		}
	}
}

func fetch(client *http.Client, url string) (string, bool) {
	resp, err := client.Get(url)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false
	}
	return string(data), true
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
