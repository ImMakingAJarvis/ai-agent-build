// image-gen-worker is the image.generate capability worker (plan task
// EVB-20): the GPU placement behind cap.image.generate. It consumes
// cap.image.generate, generates the image on the art service
// (scripts/art_service.py — Z-Image-Turbo, reference photos via
// FLUX.2 [klein], port 8120), copies the PNG into the request's OutDir
// (the shared artifacts root, so the requester can read it), and replies
// with a Result whose Artifacts point at the file (a reference, not the
// bytes — invariant 7) and whose Data carries the seed/size/timing the
// requester's caption is built from.
//
// It runs on the capworker SDK with the catalog's placement (config/
// capabilities.yaml, "image.generate"): location gpu — consumer priority 10,
// so the best free worker takes the request — and needs_brain_out: the
// 4090's VRAM must be free of the brain, so in practice requests arrive
// during the brain's swap window (docs/brain-swap.md), when the art
// container is up and the model is loaded. A resourced lease (plan §3.6)
// makes that window explicit: the worker's 12 GB VRAM lease is granted
// only once the brain's standing lease has left the ledger (the swap
// window), held while the model is loaded, and released on unload — so
// the worker consumes only when the 4090 is actually free.
//
// Request input (envelope v2, Input map):
//
//	prompt  string  required — the image to generate
//	size    string  optional — "1024x576" (pixels) or "16x9" (aspect)
//	refs    []string optional — staged reference names (the requester
//	          staged the files in the art service's refs directory); routes
//	          the generation to the reference-capable model
//
// A request without a usable prompt is a bad message: the handler errors
// on every attempt, the retry budget is burned, and the message lands on
// the dead queue with a reason (invariant 9 — nothing is silently acked).
//
// Configuration (env):
//
//	RABBITMQ_URL         the broker. Default amqp://jarvis:jarvis-local@localhost:5672.
//	ART_URL              the art service base URL. Default http://127.0.0.1:8120
//	                     (the compose service points it at http://art:8120).
//	IMAGEGEN_ART_WAIT_S  how long to wait for the art model to load before
//	                     giving up on one attempt. Default 900 (the model
//	                     is tens of GB and takes minutes even with the GPU
//	                     free) — the same bar the old jarvis-photo used.
//	IMAGEGEN_OUT_DIR     artifact root for requests without an OutDir.
//	                     Default ~/.local/share/jarvis/artifacts.
//	IMAGEGEN_METRICS_ADDR serve Prometheus /metrics here. Default: off.
//	RESOURCED_URL        the resourced lease broker (plan §3.6); the
//	                     worker gates its consuming on a lease from it.
//	                     Default: off (un-gated, the pre-lease behavior).
//	CAPABILITIES_FILE    the capability catalog the lease's memory spec
//	                     comes from. Needed when RESOURCED_URL is set.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/art"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/capworker"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

const (
	capability = "image.generate"
	workerName = "image.generate@gpu/image-gen-worker"
	model      = "zimage-turbo"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// connectWithRetry dials the broker, backing off until it is reachable
// (the compose stack may bring rabbitmq up a little after the worker).
func connectWithRetry(ctx context.Context) (*queue.Conn, error) {
	backoff := time.Second
	for {
		conn, err := queue.NewConn()
		if err == nil {
			return conn, nil
		}
		log.Printf("image-gen-worker: broker not ready: %v", err)
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

// handler does one image.generate request on the art service.
type handler struct {
	art     *art.Client
	artWait time.Duration
}

// Handle generates the requested image and returns the Result. The request's
// OutDir is already filled in by the SDK; the PNG is copied there and the
// Result's Artifacts point at the copy.
func (h handler) Handle(ctx context.Context, req queue.Message) (queue.Result, error) {
	prompt, _ := req.Input["prompt"].(string)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		// A bad message: it will fail every attempt and dead-letter with
		// this reason (the retry budget is the SDK's, not ours).
		return queue.Result{}, fmt.Errorf("image.generate: missing or empty Input[\"prompt\"]")
	}
	size, _ := req.Input["size"].(string)
	refs := refsFromInput(req.Input["refs"])
	log.Printf("image-gen-worker: %s: generating (%q, size %q, %d refs)", req.CorrelationID, prompt, size, len(refs))

	if err := h.ensureArt(ctx); err != nil {
		return queue.Result{}, err
	}

	var img *art.Image
	var err error
	if len(refs) > 0 {
		img, err = h.art.GenerateRef(ctx, prompt, size, refs)
	} else {
		img, err = h.art.Generate(ctx, prompt, size)
	}
	if err != nil {
		return queue.Result{}, err
	}

	if err := os.MkdirAll(req.OutDir, 0o755); err != nil {
		return queue.Result{}, fmt.Errorf("mkdir %s: %w", req.OutDir, err)
	}
	// The art service keeps its own copy under its OUT_DIR (that is the
	// gallery); the artifact is the worker's copy in the shared artifacts
	// root, which is what the Result references.
	name := filepath.Base(img.Path)
	dest := filepath.Join(req.OutDir, name)
	b, err := h.art.FetchImage(ctx, name)
	if err != nil {
		return queue.Result{}, err
	}
	if err := os.WriteFile(dest, b, 0o644); err != nil {
		return queue.Result{}, fmt.Errorf("write %s: %w", dest, err)
	}

	return queue.Result{
		Status:    queue.ResultOK,
		Artifacts: []string{dest},
		Summary:   fmt.Sprintf("generated %dx%d (seed %d, %.0fs)", img.Width, img.Height, img.Seed, img.Seconds),
		Data: map[string]any{
			"seed":     img.Seed,
			"width":    img.Width,
			"height":   img.Height,
			"steps":    img.Steps,
			"seconds":  img.Seconds,
			"art_path": img.Path,
			"gallery":  art.GalleryURL(),
		},
	}, nil
}

// ensureArt makes sure the art service is up and its model loaded before a
// generation: it probes GET /health and, when the service is up but the
// model is still loading, waits (bounded by artWait) — the same wait the
// old jarvis-photo did. A service that is down is an error for this
// attempt: it comes up with the swap window, and if it is not there the
// SDK burns an attempt (retry, then the message dead-letters with a
// reason) rather than this worker blocking forever.
func (h handler) ensureArt(ctx context.Context) error {
	loaded, err := h.health()
	if err != nil {
		return &art.ErrUnavailable{Detail: "art service not reachable (is the swap window open?)"}
	}
	if loaded {
		return nil
	}
	deadline := time.Now().Add(h.artWait)
	log.Printf("image-gen-worker: art service is up, model not loaded — waiting (up to %s)", h.artWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		if loaded, err := h.health(); err == nil && loaded {
			return nil
		}
	}
	return &art.ErrUnavailable{Detail: fmt.Sprintf("art model not loaded after %s", h.artWait)}
}

// health probes the art service's /health (10s bound) and reports whether
// the model is loaded; err is non-nil when it cannot be reached.
func (h handler) health() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.art.Health(ctx)
}

// refsFromInput extracts the optional refs input (JSON decodes lists as
// []any; a direct []string is accepted too).
func refsFromInput(v any) []string {
	switch r := v.(type) {
	case []string:
		return r
	case []any:
		out := make([]string, 0, len(r))
		for _, x := range r {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func artWait() time.Duration {
	if v := os.Getenv("IMAGEGEN_ART_WAIT_S"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
		log.Printf("image-gen-worker: bad IMAGEGEN_ART_WAIT_S %q — using default", v)
	}
	return 15 * time.Minute
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("image-gen-worker: ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := connectWithRetry(ctx)
	if err != nil {
		log.Fatalf("image-gen-worker: connect to broker: %v", err)
	}
	defer conn.Close()

	events, err := queue.NewEventPublisher(conn, "image-gen-worker")
	if err != nil {
		log.Printf("image-gen-worker: event publisher: %v (events disabled)", err)
		events = nil
	}
	if events != nil {
		defer events.Close()
	}

	// Lease gating (plan §3.6): the gpu placement's lease is the
	// needs_brain_out story made explicit — 12 GB of VRAM is granted
	// only while the brain's standing lease is out of the ledger (the
	// swap window). No broker (RESOURCED_URL unset) or a catalog problem
	// = un-gated, the pre-lease behavior.
	leaseBroker, leaseSpec, lerr := capworker.LeaseFor(
		os.Getenv("RESOURCED_URL"), os.Getenv("CAPABILITIES_FILE"),
		capability, "gpu", workerName)
	if lerr != nil {
		log.Printf("image-gen-worker: lease: %v (running un-gated)", lerr)
	}

	w := capworker.New(capworker.Config{
		Cap:         capability,
		Name:        workerName,
		Model:       model,
		Location:    "gpu", // consumer priority 10: the best free worker takes the request
		Conn:        conn,
		Events:      events,
		LeaseBroker: leaseBroker,
		LeaseSpec:   leaseSpec,
		MetricsAddr: os.Getenv("IMAGEGEN_METRICS_ADDR"),
		Handler:     handler{art: art.New(), artWait: artWait()},
		OutDirBase:  envOr("IMAGEGEN_OUT_DIR", filepath.Join(os.Getenv("HOME"), ".local", "share", "jarvis", "artifacts")),
		Log:         log.Default(),
	})
	if err := w.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("image-gen-worker: run: %v", err)
	}
	log.Printf("image-gen-worker: stopped")
}
