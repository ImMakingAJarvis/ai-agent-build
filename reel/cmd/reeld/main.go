// Command reeld is the Reel media server: one binary, SQLite storage,
// REST API defined by openapi.yaml, and the scan queue consumer (the
// scan job pauses under the gaming watchdog — internal/reelq).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/api"
	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/db"
	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/reelq"
	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/scan"
	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/tmdb"
)

// Config is the server configuration. File fields are JSON names; env vars
// REEL_ADDR / REEL_DB / RABBITMQ_URL / REEL_RESOURCED / TMDB_API_KEY /
// REEL_CACHE_DIR override the file values.
type Config struct {
	Addr      string `json:"addr"`
	DBPath    string `json:"db_path"`
	AMQPURL   string `json:"amqp_url"`
	Resourced string `json:"resourced"` // resourced HTTP base (GET /state)
	TmdbKey   string `json:"tmdb_key"`  // TMDB v3 API key (env-only; never in the repo)
	CacheDir  string `json:"cache_dir"` // on-disk metadata/artwork cache
}

func loadConfig() (Config, error) {
	cfg := Config{
		Addr:      ":8122",
		DBPath:    "reel.db",
		AMQPURL:   "amqp://jarvis:jarvis-local@localhost:5672",
		Resourced: "http://172.17.0.1:9104",
	}
	if f := os.Getenv("REEL_CONFIG"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return cfg, err
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, err
		}
	}
	if v := os.Getenv("REEL_ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := os.Getenv("REEL_DB"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("RABBITMQ_URL"); v != "" {
		cfg.AMQPURL = v
	}
	if v := os.Getenv("REEL_RESOURCED"); v != "" {
		cfg.Resourced = v
	}
	if v := os.Getenv("REEL_TMDB_KEY"); v != "" {
		cfg.TmdbKey = v
	}
	if v := os.Getenv("TMDB_API_KEY"); v != "" {
		cfg.TmdbKey = v
	}
	if v := os.Getenv("REEL_CACHE_DIR"); v != "" {
		cfg.CacheDir = v
	}
	if cfg.CacheDir == "" {
		// The NAS media mounts are read-only, so the cache lives on this
		// box next to the database file (documented in the README).
		cfg.CacheDir = filepath.Join(filepath.Dir(cfg.DBPath), "cache")
	}
	return cfg, nil
}

// queueRunner is the api.ScanRunner: it enqueues on the publisher once the
// queue bootstrap has connected, and refuses (502 at the API) until then.
type queueRunner struct {
	mu  sync.Mutex
	pub *reelq.Publisher
}

func (q *queueRunner) EnqueueScan(ctx context.Context, libraryID, scanID int64) error {
	q.mu.Lock()
	pub := q.pub
	q.mu.Unlock()
	if pub == nil {
		return errors.New("scan queue not connected yet")
	}
	return pub.PublishScan(ctx, libraryID, scanID)
}

func (q *queueRunner) setPublisher(p *reelq.Publisher) {
	q.mu.Lock()
	q.pub = p
	q.mu.Unlock()
}

// scanHandler is the consumer's job handler: run the scan for the library
// (fetching TMDB metadata through f when one is wired) and close the scan
// row out with its counters or the error.
func scanHandler(d *db.DB, f scan.Fetcher) reelq.Handler {
	return func(ctx context.Context, j reelq.ScanJob) error {
		lib, err := d.GetLibrary(j.LibraryID)
		if err != nil {
			return fmt.Errorf("library %d: %w", j.LibraryID, err)
		}
		if err := d.StartScan(j.ScanID); err != nil {
			return err
		}
		res, err := scan.RunWith(ctx, d, lib, f)
		if err != nil {
			_ = d.FinishScan(j.ScanID, db.ScanError, res.Added, res.Updated, res.Skipped, err.Error())
			return err
		}
		return d.FinishScan(j.ScanID, db.ScanDone, res.Added, res.Updated, res.Skipped, "")
	}
}

func main() {
	log.SetPrefix("reeld: ")
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer func() { _ = d.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var fetcher scan.Fetcher
	cache, err := tmdb.NewCache(cfg.CacheDir)
	if err != nil {
		log.Fatalf("open cache dir %s: %v", cfg.CacheDir, err)
	}
	if cfg.TmdbKey != "" {
		fetcher = tmdb.New(cfg.TmdbKey, cache)
		log.Printf("TMDB metadata enabled (cache %s)", cfg.CacheDir)
	} else {
		log.Printf("no TMDB key (TMDB_API_KEY / REEL_TMDB_KEY) — scans run without metadata")
	}

	runner := &queueRunner{}
	go bootQueue(ctx, cfg, d, runner, fetcher)

	srv := api.NewWith(d, runner, cfg.CacheDir)
	log.Printf("listening on %s (db %s)", cfg.Addr, cfg.DBPath)
	if err := srv.Listen(cfg.Addr); err != nil {
		log.Fatal(err)
	}
}

// bootQueue connects to the broker (retrying, so a reeld that starts
// before RabbitMQ does not lose its queue for good), then runs the scan
// consumer and the stream watcher for the life of the process. A game
// stream already active at startup is picked up by the /state poll, so
// the consumer starts paused when it should be.
func bootQueue(ctx context.Context, cfg Config, d *db.DB, runner *queueRunner, fetcher scan.Fetcher) {
	var pub *reelq.Publisher
	for {
		var err error
		pub, err = reelq.NewPublisher(cfg.AMQPURL)
		if err == nil {
			break
		}
		log.Printf("scan queue: broker unavailable (%v) — retrying in 2 s", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	// Bind before the API can enqueue: a publish that lands before the
	// consumer's bind would be dropped by the exchange.
	if err := reelq.EnsureScanQueue(cfg.AMQPURL); err != nil {
		log.Printf("scan queue: declare/bind: %v (the consumer re-binds on (re)start)", err)
	}
	runner.setPublisher(pub)
	defer func() { _ = pub.Close() }()

	cons := reelq.NewConsumer(cfg.AMQPURL, scanHandler(d, fetcher))
	if reelq.StreamActive(ctx, cfg.Resourced) {
		log.Printf("a game stream is active — the scan queue starts paused")
		cons.Pause()
	}
	go func() {
		if err := cons.WatchStreams(ctx); err != nil && ctx.Err() == nil {
			log.Printf("scan queue: stream watcher: %v", err)
		}
	}()
	if err := cons.Start(ctx); err != nil && ctx.Err() == nil {
		log.Printf("scan queue: %v", err)
	}
}
