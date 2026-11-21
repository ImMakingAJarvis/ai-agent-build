package reelq_test

// Queue E2E: a scan POSTed to the API runs as a real job on the local
// RabbitMQ (the same queue reeld consumes) and the consumer pauses while
// a game stream is active. Runs only where a broker is reachable — CI
// has none, so the test skips there. Stream events are published on a
// private test exchange (SetEventExchange), never the live "events" one,
// so a fake game start cannot pause a real stack worker.
//
// The media walked is the checked-in test library (reel/testdata) plus a
// scratch directory in t.TempDir — never the real NAS library.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/api"
	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/db"
	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/reelq"
	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/scan"
)

const testEventExchange = "reel.test.events"

func brokerURL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("RABBITMQ_URL")
	if raw == "" {
		raw = "amqp://jarvis:jarvis-local@localhost:5672"
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Skipf("bad RABBITMQ_URL %q: %v", raw, err)
	}
	if u.Host == "" {
		t.Skipf("RABBITMQ_URL %q has no host:port to probe", raw)
	}
	conn, err := amqp.Dial(raw)
	if err != nil {
		t.Skipf("broker unreachable, skipping queue E2E: %v", err)
	}
	_ = conn.Close()
	return raw
}

// scanHandler mirrors cmd/reeld's consumer handler.
func scanHandler(d *db.DB) reelq.Handler {
	return func(_ context.Context, j reelq.ScanJob) error {
		lib, err := d.GetLibrary(j.LibraryID)
		if err != nil {
			return err
		}
		if err := d.StartScan(j.ScanID); err != nil {
			return err
		}
		res, err := scan.Run(d, lib)
		if err != nil {
			_ = d.FinishScan(j.ScanID, db.ScanError, res.Added, res.Updated, res.Skipped, err.Error())
			return err
		}
		return d.FinishScan(j.ScanID, db.ScanDone, res.Added, res.Updated, res.Skipped, "")
	}
}

func TestScanQueueEndToEnd(t *testing.T) {
	burl := brokerURL(t)

	d, err := db.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	movies, err := d.CreateLibrary(db.Library{Name: "E2E Movies", Path: "../../testdata/movies", Type: "movies"})
	if err != nil {
		t.Fatal(err)
	}
	shows, err := d.CreateLibrary(db.Library{Name: "E2E Shows", Path: "../../testdata/shows", Type: "shows"})
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	writeMovie(t, scratch, "Pause Movie (2021)", "pause.movie.2021.1080p.mp4")
	scratchLib, err := d.CreateLibrary(db.Library{Name: "E2E Scratch", Path: scratch, Type: "movies"})
	if err != nil {
		t.Fatal(err)
	}

	// The scan queue is durable and shared with a live reeld: start from a
	// clean queue so a stale job from an earlier run cannot confuse the
	// counts.
	purge := func() {
		t.Helper()
		conn, err := amqp.Dial(burl)
		if err != nil {
			t.Fatalf("re-dial for purge: %v", err)
		}
		ch, err := conn.Channel()
		if err == nil {
			if _, err := ch.QueueDeclare(reelq.ScanQueue, true, false, false, false, nil); err == nil {
				_, _ = ch.QueuePurge(reelq.ScanQueue, false)
			}
		}
		_ = conn.Close()
	}
	purge()
	t.Cleanup(purge)
	// Bind before the first publish: the exchange drops a message that
	// arrives before the consumer's bind is registered.
	if err := reelq.EnsureScanQueue(burl); err != nil {
		t.Fatalf("ensure scan queue: %v", err)
	}

	pub, err := reelq.NewPublisher(burl)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	t.Cleanup(func() { _ = pub.Close() })

	cons := reelq.NewConsumer(burl, scanHandler(d))
	cons.SetEventExchange(testEventExchange)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = cons.Start(ctx) }()
	go func() { _ = cons.WatchStreams(ctx) }()

	ts := httptest.NewServer(api.NewWithRunner(d, publisher{pub}).Handler())
	defer ts.Close()

	postScan := func(t *testing.T, libID int64) {
		t.Helper()
		res, err := http.Post(ts.URL+fmt.Sprintf("/api/v1/libraries/%d/scan", libID), "application/json", nil)
		if err != nil {
			t.Fatalf("POST scan: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusAccepted {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("POST scan: status %d (%s)", res.StatusCode, b)
		}
	}
	getScan := func(t *testing.T, libID int64) map[string]any {
		t.Helper()
		res, err := http.Get(ts.URL + fmt.Sprintf("/api/v1/libraries/%d/scan", libID))
		if err != nil {
			t.Fatalf("GET scan: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("GET scan: status %d (%s)", res.StatusCode, b)
		}
		var out map[string]any
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	waitForScan := func(t *testing.T, libID int64, want string, timeout time.Duration) map[string]any {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			st := getScan(t, libID)
			if st["state"] == want {
				return st
			}
			if st["state"] == "error" && want != "error" {
				t.Fatalf("scan for library %d errored: %v", libID, st["error"])
			}
			if time.Now().After(deadline) {
				t.Fatalf("scan for library %d still %q, want %q", libID, st["state"], want)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	listItems := func(t *testing.T, libID int64) []map[string]any {
		t.Helper()
		res, err := http.Get(ts.URL + fmt.Sprintf("/api/v1/items?library_id=%d", libID))
		if err != nil {
			t.Fatalf("GET items: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("GET items: status %d (%s)", res.StatusCode, b)
		}
		var out []map[string]any
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// publishStreamEvent emits one resourced-shaped event on the private
	// test exchange.
	publishStreamEvent := func(t *testing.T, routing string, game bool) {
		t.Helper()
		conn, err := amqp.Dial(burl)
		if err != nil {
			t.Fatalf("dial for events: %v", err)
		}
		defer func() { _ = conn.Close() }()
		ch, err := conn.Channel()
		if err != nil {
			t.Fatal(err)
		}
		if err := ch.ExchangeDeclare(testEventExchange, "topic", true, false, false, false, nil); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"kind": "stream", "state": routing, "data": map[string]any{"game": game, "stream": "e2e"}})
		if err := ch.PublishWithContext(ctx, testEventExchange, "stream."+routing, false, false, amqp.Publishing{
			ContentType: "application/json",
			Body:        body,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// --- a real enqueued scan fills the API (acceptance: items appear) ---
	postScan(t, movies.ID)
	postScan(t, shows.ID)
	waitForScan(t, movies.ID, "done", 30*time.Second)
	stShows := waitForScan(t, shows.ID, "done", 30*time.Second)

	movieItems := listItems(t, movies.ID)
	if len(movieItems) != 3 {
		t.Fatalf("movies: %d items, want 3: %v", len(movieItems), movieItems)
	}
	for _, it := range movieItems {
		if it["type"] != "movie" || it["title"] == "" || it["year"] == nil || it["year"].(float64) < 1900 {
			t.Errorf("movie item missing title/year: %v", it)
		}
	}
	showItems := listItems(t, shows.ID)
	if len(showItems) != 6 { // 2 seasons + 4 episodes
		t.Fatalf("shows: %d items, want 6: %v", len(showItems), showItems)
	}
	var episodes, seasons int
	for _, it := range showItems {
		switch it["type"] {
		case "episode":
			episodes++
			if it["season"] == nil || it["episode"] == nil || it["parent_id"] == nil {
				t.Errorf("episode missing season/episode/parent: %v", it)
			}
		case "season":
			seasons++
		}
	}
	if episodes != 4 || seasons != 2 {
		t.Fatalf("shows: %d episodes / %d seasons, want 4 / 2", episodes, seasons)
	}
	if stShows["added"].(float64) != 6 {
		t.Errorf("shows scan added %v, want 6", stShows["added"])
	}

	// --- the pause half: a game stream holds the queue ---
	postScan(t, scratchLib.ID)
	waitForScan(t, scratchLib.ID, "done", 30*time.Second)
	if got := listItems(t, scratchLib.ID); len(got) != 1 {
		t.Fatalf("scratch: %d items, want 1: %v", len(got), got)
	}

	publishStreamEvent(t, "started", true)
	deadline := time.Now().Add(5 * time.Second)
	for !cons.IsPaused() {
		if time.Now().After(deadline) {
			t.Fatal("consumer did not pause on the game stream event")
		}
		time.Sleep(50 * time.Millisecond)
	}

	writeMovie(t, scratch, "Second Film (2022)", "second.film.2022.mp4")
	postScan(t, scratchLib.ID)          // enqueues behind the hold
	time.Sleep(1500 * time.Millisecond) // a running consumer would be long done
	if st := getScan(t, scratchLib.ID); st["state"] != "queued" {
		t.Fatalf("scan state %v while a game stream is active, want queued", st["state"])
	}
	if got := listItems(t, scratchLib.ID); len(got) != 1 {
		t.Fatalf("scan ran while paused: %d items, want still 1", len(got))
	}

	publishStreamEvent(t, "stopped", true)
	deadline = time.Now().Add(5 * time.Second)
	for cons.IsPaused() {
		if time.Now().After(deadline) {
			t.Fatal("consumer did not resume on the stream stop")
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := waitForScan(t, scratchLib.ID, "done", 30*time.Second)
	if st["added"].(float64) != 1 {
		t.Errorf("rescan after resume added %v, want 1", st["added"])
	}
	got := listItems(t, scratchLib.ID)
	if len(got) != 2 {
		t.Fatalf("after resume: %d items, want 2: %v", len(got), got)
	}
	found := false
	for _, it := range got {
		if it["title"] == "Second Film" && it["year"].(float64) == 2022 {
			found = true
		}
	}
	if !found {
		t.Errorf("Second Film (2022) not in items: %v", got)
	}
}

// publisher adapts *reelq.Publisher to the api.ScanRunner interface
// (the API-facing name for enqueuing a scan job).
type publisher struct{ p *reelq.Publisher }

func (a publisher) EnqueueScan(ctx context.Context, libraryID, scanID int64) error {
	return a.p.PublishScan(ctx, libraryID, scanID)
}

// writeMovie adds one movie-layout file ("Name (Year)/file") to dir. The
// content is irrelevant: the scanner stats files, it never opens them.
func writeMovie(t *testing.T, root, dir, file string) {
	t.Helper()
	p := filepath.Join(root, dir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, file), []byte("not a real video"), 0o644); err != nil {
		t.Fatal(err)
	}
}
