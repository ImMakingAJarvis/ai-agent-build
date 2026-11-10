// jarvis-photo runs one queued photo job — as code, no model round. It is
// self-sufficient: it reads the job's (brain-expanded) prompt, starts the art
// service if it is down, waits for the model to load, generates the image
// (the art service saves it to disk and the gallery at the same time),
// delivers the finished PNG to the owner's Discord DM, and — if the job
// carries a trailing instruction — queues that as a new job with the image
// path. The agentjob Runner runs it directly for photo-tagged jobs
// (Runner.runPhoto).
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
)

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

	// A few minutes: the art service waits up to ~90s for free VRAM, then an
	// image takes tens of seconds; a reference generation can take longer
	// (the first call after a restart also loads the FLUX.2 [klein] model).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	img, err := makePhoto(ctx, dataDir, id, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: generate failed: %v\n", err)
		return 1
	}

	caption := fmt.Sprintf("**%s**\nseed %d · %dx%d · %.0fs\nGallery: %s",
		truncate(prompt, 600), img.Seed, img.Width, img.Height, img.Seconds, art.GalleryURL())
	if err := deliver(img.Path, caption); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: photo made (%s) but delivery failed: %v\n", img.Path, err)
		return 1
	}
	// The image reached Discord: mark the job delivered so the run wrapper
	// records the run done (a run that ends without this marker is a
	// failure the owner can re-queue).
	if err := photo.MarkSent(dataDir, id); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: delivery marker: %v\n", err)
		return 1
	}
	fmt.Println("sent " + filepath.Base(img.Path))

	// The job's trailing instruction, if it has one: queue a new job that
	// gets the generated image. A failure here fails the job (re-queueable);
	// the re-run regenerates and re-sends, which is the same retry contract
	// the delivery itself already has.
	return queueFollowup(dataDir, id, img.Path)
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

// makePhoto reads the job's size token (if any) and generates the image on
// the art service. The size sidecar is optional: jobs queued before sizes
// existed have none, and the service default (square 1024x1024) applies.
//
// A reference sidecar (the owner's photo, pasted with the prompt) routes the
// generation to the reference-capable model: the photo is staged in the art
// service's refs directory first, and the service loads it by name so the
// result keeps the subject from the photo.
func makePhoto(ctx context.Context, dataDir, id, prompt string) (*art.Image, error) {
	client := art.New()
	size, err := photo.ReadSize(dataDir, id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read size: %w", err)
	}
	if ref, err := photo.RefFile(dataDir, id); err == nil {
		name, err := stageRef(ref)
		if err != nil {
			return nil, err
		}
		return client.GenerateRef(ctx, prompt, size, []string{name})
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reference sidecar: %w", err)
	}
	return client.Generate(ctx, prompt, size)
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
