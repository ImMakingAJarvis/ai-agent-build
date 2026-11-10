// jarvis-photo runs one queued photo job — as code, no model round. Since
// EVB-20 it is a thin forwarder for the image.generate capability: it
// reads the job's (brain-expanded) prompt, stands the art service up if it
// is down, publishes a cap.image.generate request (correlation id = the
// job id, Result back on flow.photo-<job-id>), waits for the Result,
// delivers the finished PNG to the owner's Discord DM exactly as before,
// marks the job sent, and queues the job's optional follow-up.
//
// The generation itself runs in the image-gen-worker (cmd/image-gen-worker)
// on the capworker SDK, which makes the run idempotent by correlation id:
// a re-run of the job resubmits the same correlation id, and the worker's
// .done marker re-replies the stored Result instead of regenerating.
//
// The !photo UX is unchanged: same prompt sidecars, same caption
// (prompt, seed, size, seconds, gallery), same delivery via
// jarvis-discord, same follow-up handling.
//
//	jarvis-photo make --job <id>
//
// Exit 0 when the image was generated, sent, and any follow-up was queued;
// non-zero otherwise (the run records a failure and the job can be
// re-queued).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/art"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photo"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

const capImageGenerate = "image.generate"

func main() {
	os.Exit(run(os.Args))
}

func run(args []string) int {
	if len(args) != 4 || args[1] != "make" || args[2] != "--job" {
		fmt.Fprintln(os.Stderr, "usage: jarvis-photo make --job <id>")
		return 2
	}
	id := args[3]
	if !agentjob.ValidID(id) {
		fmt.Fprintf(os.Stderr, "jarvis-photo: bad job id %q\n", id)
		return 2
	}

	dataDir := agentjob.DataDir()
	prompt, err := photo.ReadPrompt(dataDir, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: cannot read prompt for %s: %v\n", id, err)
		return 1
	}
	if prompt == "" {
		fmt.Fprintf(os.Stderr, "jarvis-photo: empty prompt for %s\n", id)
		return 1
	}

	// Drop any delivery marker from an earlier run: only this run's
	// delivery can count, so a failed re-run fails the job (re-queueable)
	// instead of being marked done on the old marker.
	if err := photo.ClearSent(dataDir, id); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "jarvis-photo: clear sent marker: %v\n", err)
		return 1
	}

	// The executor must stand up the art service itself: nothing else in the
	// pipeline is guaranteed to have started it.
	if err := ensureArt(); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: %v\n", err)
		return 1
	}

	// A few minutes: the worker waits (bounded) for the model, the art
	// service waits up to ~90s for free VRAM, then an image takes tens of
	// seconds; a reference generation can take longer.
	ctx, cancel := context.WithTimeout(context.Background(), resultWait())
	defer cancel()
	res, err := submitAndWait(ctx, dataDir, id, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: generate failed: %v\n", err)
		return 1
	}
	path, caption, err := photoResult(res, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: %v\n", err)
		return 1
	}

	if err := deliver(path, caption); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: photo made (%s) but delivery failed: %v\n", path, err)
		return 1
	}
	// The image reached Discord: mark the job delivered so the run wrapper
	// records the run done (a run that ends without this marker is a
	// failure the owner can re-queue).
	if err := photo.MarkSent(dataDir, id); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: delivery marker: %v\n", err)
		return 1
	}
	fmt.Println("sent " + filepath.Base(path))

	// The job's trailing instruction, if it has one: queue a new job that
	// gets the generated image. A failure here fails the job (re-queueable);
	// the re-run gets the stored result (the worker's idempotency marker)
	// and re-sends, which is the same retry contract the delivery itself
	// already has.
	return queueFollowup(dataDir, id, path)
}

// buildRequest assembles the cap.image.generate input for a photo job: the
// prompt, the optional size sidecar (legacy jobs have none), and the
// reference sidecar — staged in the art service's refs directory when
// present, so the generation routes to the reference-capable model.
func buildRequest(dataDir, id, prompt string) (map[string]any, error) {
	input := map[string]any{"prompt": prompt}
	size, err := photo.ReadSize(dataDir, id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read size: %w", err)
	}
	if size != "" {
		input["size"] = size
	}
	if ref, err := photo.RefFile(dataDir, id); err == nil {
		name, err := stageRef(ref)
		if err != nil {
			return nil, err
		}
		input["refs"] = []string{name}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reference sidecar: %w", err)
	}
	return input, nil
}

// submitAndWait publishes the cap.image.generate request (correlation id =
// the job id, so a re-run is the same request) and blocks until the
// worker's Result lands on the job's flow queue. The Result is acked off
// the queue when consumed; the queue itself is deleted afterwards (it is
// re-declared on demand if the job is re-run).
func submitAndWait(ctx context.Context, dataDir, id, prompt string) (*queue.Result, error) {
	input, err := buildRequest(dataDir, id, prompt)
	if err != nil {
		return nil, err
	}
	conn, err := queue.NewConn()
	if err != nil {
		return nil, fmt.Errorf("connect to broker: %w", err)
	}
	defer conn.Close()

	replyTo := queue.FlowQueueName("photo-" + id) // flow.photo-<job-id>
	qname := queue.CapQueueName(capImageGenerate)
	if err := conn.EnsureCapQueue(qname); err != nil {
		return nil, fmt.Errorf("ensure %s: %w", qname, err)
	}
	// Declare the Result queue up front so the consumer's Get always finds
	// it and a Result published before we look is held on the broker (the
	// worker re-declares it too — a no-op).
	if err := conn.EnsureFlowQueue(replyTo); err != nil {
		return nil, fmt.Errorf("ensure %s: %w", replyTo, err)
	}
	req := queue.Message{
		ID:            fmt.Sprintf("%s-%d", id, time.Now().UnixMilli()),
		Type:          "capability",
		CreatedAt:     time.Now().UTC(),
		Source:        "jarvis-photo",
		SchemaVersion: 2,
		Capability:    capImageGenerate,
		ReplyTo:       replyTo,
		CorrelationID: id,
		Input:         input,
		Deadline:      ptrTime(time.Now().Add(resultWait())),
	}
	if err := conn.Publish(req, qname+".default"); err != nil {
		return nil, fmt.Errorf("publish %s: %w", qname, err)
	}
	fmt.Printf("submitted image.generate (corr=%s, reply-to=%s)\n", id, req.ReplyTo)

	ch, err := conn.NewChannel()
	if err != nil {
		return nil, fmt.Errorf("result channel: %w", err)
	}
	defer ch.Close()
	for {
		del, ok, err := ch.Get(req.ReplyTo, false)
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", req.ReplyTo, err)
		}
		if !ok {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("timed out after %s waiting for the image result on %s", resultWait(), req.ReplyTo)
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if res, match := matchResult(del.Body, id); match {
			if aerr := del.Ack(false); aerr != nil {
				return nil, fmt.Errorf("ack result: %w", aerr)
			}
			// Tidy up: the flow queue is this job's and is re-declared on
			// demand if the job is re-run. Best effort — a leftover queue
			// is harmless.
			if derr := conn.DeleteFlow(req.ReplyTo); derr != nil {
				fmt.Fprintf(os.Stderr, "jarvis-photo: delete flow %s: %v\n", req.ReplyTo, derr)
			}
			return res, nil
		}
		// Not a Result for this correlation: leave it in the queue and
		// keep waiting (the queue is this job's, so this should not happen).
		if nerr := del.Nack(false, true); nerr != nil {
			return nil, fmt.Errorf("requeue foreign message: %w", nerr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// matchResult reports whether body is a Result (a capability reply, not a
// plain job message) for correlation id corr.
func matchResult(body []byte, corr string) (*queue.Result, bool) {
	res, err := queue.DecodeResult(body)
	if err != nil || res.Status == "" || res.CorrelationID != corr {
		return nil, false
	}
	return &res, true
}

// photoResult turns the worker's Result into the artifact path and the
// Discord caption. The caption keeps the historical format — prompt, seed,
// size, seconds, gallery — built from the Result's Data.
func photoResult(res *queue.Result, prompt string) (string, string, error) {
	if res.Status != queue.ResultOK {
		return "", "", fmt.Errorf("image result status %s: %s", res.Status, or(res.Error, res.Summary))
	}
	if len(res.Artifacts) == 0 {
		return "", "", errors.New("image result has no artifacts")
	}
	path := res.Artifacts[0]
	if _, err := os.Stat(path); err != nil {
		return "", "", fmt.Errorf("artifact %s not readable: %w", path, err)
	}
	seed, _ := res.Data["seed"].(float64)
	width, _ := res.Data["width"].(float64)
	height, _ := res.Data["height"].(float64)
	seconds, _ := res.Data["seconds"].(float64)
	caption := fmt.Sprintf("**%s**\nseed %d · %dx%d · %.0fs\nGallery: %s",
		truncate(prompt, 600), int64(seed), int(width), int(height), seconds, art.GalleryURL())
	return path, caption, nil
}

// ensureArt makes sure the art service is up and its model is loaded before
// a generation. It probes GET /health: if the service is unreachable it
// starts the art container (docker compose up -d art, the same compose
// project the swap window uses) and waits; if it is up but the model is not
// loaded yet it waits for "loaded":true.
func ensureArt() error {
	loaded, reachable := probeArt()
	if !reachable {
		if err := startArtService(); err != nil {
			return fmt.Errorf("art service is down and could not be started: %w", err)
		}
	} else if !loaded {
		fmt.Println("art service is up, model not loaded — waiting")
	}
	return waitArtLoaded()
}

// probeArt asks the art service for /health and reports whether it answered
// and whether the model is loaded.
func probeArt() (loaded, reachable bool) {
	body, ok := httpGetJSON(artURL() + "/health")
	if !ok {
		return false, false
	}
	var h struct {
		Loaded bool `json:"loaded"`
	}
	if json.Unmarshal(body, &h) != nil {
		return false, true
	}
	return h.Loaded, true
}

// startArtService starts the art container if it is not running. It runs
// `docker compose up -d art` in the repo (SWAP_REPO, same default as
// jarvis-swap) — a no-op when the container is already up.
func startArtService() error {
	repo := os.Getenv("SWAP_REPO")
	if repo == "" {
		repo = "/home/agent/ai-agent-build"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "compose", "up", "-d", "art")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose up -d art: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("started the art container (docker compose up -d art)")
	return nil
}

// waitArtLoaded polls the art service's /health until the model reports
// loaded (or PHOTO_ART_WAIT_S, default 15 min — the model is tens of GB and
// takes minutes to load even with the GPU free).
func waitArtLoaded() error {
	waitS := envInt("PHOTO_ART_WAIT_S", 900)
	deadline := time.Now().Add(time.Duration(waitS) * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		body, ok := httpGetJSON(artURL() + "/health")
		if ok {
			last = body
			var h struct {
				Loaded bool `json:"loaded"`
			}
			if json.Unmarshal(body, &h) == nil && h.Loaded {
				return nil
			}
		}
		time.Sleep(5 * time.Second)
	}
	if last != nil {
		return fmt.Errorf("art model not loaded after %ds (last health: %s)", waitS, truncate(string(last), 300))
	}
	return fmt.Errorf("art service at %s did not come up after %ds", artURL(), waitS)
}

// queueFollowup enqueues the photo job's trailing instruction as a new job,
// giving it the generated image. With no follow-up sidecar the photo job
// simply ends. {{image}} in the text is replaced with the image path; when
// the text has no placeholder, the path and the gallery URL are appended.
func queueFollowup(dataDir, photoID, imgPath string) int {
	body, err := photo.ReadFollowup(dataDir, photoID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "jarvis-photo: read follow-up: %v\n", err)
		return 1
	}
	if strings.TrimSpace(body) == "" {
		return 0
	}
	fu, err := photo.ParseFollowup(body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: bad follow-up: %v\n", err)
		return 1
	}
	instr := fu.Instructions
	if strings.Contains(instr, "{{image}}") {
		instr = strings.ReplaceAll(instr, "{{image}}", imgPath)
	} else {
		instr = fmt.Sprintf("%s\n\nGenerated image: %s\nGallery: %s",
			strings.TrimRight(instr, "\n"), imgPath, art.GalleryURL())
	}
	store, err := agentjob.Open(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: open job store: %v\n", err)
		return 1
	}
	fid := "fu-" + photoID
	spec := agentjob.Spec{
		ID:           fid,
		Title:        or(fu.Title, "follow-up for photo "+photoID),
		Instructions: instr,
		Driver:       or(fu.Driver, "local"),
		Class:        fu.Class,
		Priority:     fu.Priority,
		Source:       "photo-followup",
	}
	if _, err := store.Ingest(spec); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: queue follow-up %s: %v\n", fid, err)
		return 1
	}
	fmt.Println("queued follow-up " + fid)
	return 0
}

// stageRef copies a job's reference sidecar into the art service's refs
// directory (bind-mounted into the art container) and returns the file name
// the service loads it by. The name carries the job id and a timestamp so a
// re-run stages a fresh copy instead of reusing a stale one.
func stageRef(refPath string) (string, error) {
	b, err := os.ReadFile(refPath)
	if err != nil {
		return "", fmt.Errorf("read reference: %w", err)
	}
	dir := os.Getenv("ART_REFS_DIR")
	if dir == "" {
		dir = "/var/lib/jarvis/art/refs"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// The sidecar is <data>/photos/<id>.png; reuse the job id in the name.
	id := strings.TrimSuffix(filepath.Base(refPath), ".png")
	name := fmt.Sprintf("ref-%s-%d.png", id, time.Now().Unix())
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		return "", err
	}
	return name, nil
}

// deliver sends the image to the owner's Discord DM via jarvis-discord, which
// resolves its own token/owner config (env or ~/.config/jarvis/discord.env).
// The caption is passed as a single argument so its newlines survive.
func deliver(path, caption string) error {
	bin, err := exec.LookPath("jarvis-discord")
	if err != nil {
		bin = "/usr/local/bin/jarvis-discord"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "image", path, caption)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// httpGetJSON GETs url (10s timeout) and returns the body.
func httpGetJSON(url string) ([]byte, bool) {
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Get(url)
	if err != nil {
		return nil, false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 8192))
	if err != nil {
		return nil, false
	}
	return b, true
}

// artURL is the base URL of the art service (ART_URL, default the host
// listener) — same convention as the art client itself.
func artURL() string {
	if u := os.Getenv("ART_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return art.DefaultBase
}

// resultWait bounds how long the forwarder waits for the worker's Result
// (PHOTO_RESULT_WAIT_S, default 10 min). It is also the deadline carried on
// the request, so a worker that gives up sees the same horizon.
func resultWait() time.Duration {
	if v := os.Getenv("PHOTO_RESULT_WAIT_S"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 10 * time.Minute
}

func ptrTime(t time.Time) *time.Time { return &t }

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
