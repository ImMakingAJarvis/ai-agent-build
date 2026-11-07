// jarvis-photo runs one queued photo job. The iGPU round that the queue
// watcher runs for a photo job does exactly one thing: run this. It reads the
// job's (brain-expanded) prompt, generates the image on the art service —
// which has the 4090 free during the brain-swap window — and delivers the
// finished PNG to the owner's Discord DM.
//
//	jarvis-photo make --job <id>
//
// Exit 0 when the image was generated and sent, non-zero otherwise (the round
// reports the error and the job fails, so it can be re-queued).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	// Drop any delivery marker from an earlier round: only this round's
	// delivery can count, so a failed re-run fails the job (re-queueable)
	// instead of being marked done on the old marker.
	if err := photo.ClearSent(dataDir, id); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "jarvis-photo: clear sent marker: %v\n", err)
		return 1
	}

	// A few minutes: the art service waits up to ~90s for free VRAM, then a
	// 1024x1024 image takes tens of seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	img, err := art.New().Generate(ctx, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: generate failed: %v\n", err)
		return 1
	}

	caption := fmt.Sprintf("**%s**\nseed %d · %dx%d · %.0fs\nGallery: %s",
		truncatePrompt(prompt, 600), img.Seed, img.Width, img.Height, img.Seconds, art.GalleryURL())
	if err := deliver(img.Path, caption); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: photo made (%s) but delivery failed: %v\n", img.Path, err)
		return 1
	}
	// The image reached Discord: mark the job delivered so the run wrapper
	// records the round done (a round that ends without this marker is a
	// failure the owner can re-queue).
	if err := photo.MarkSent(dataDir, id); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-photo: delivery marker: %v\n", err)
		return 1
	}
	fmt.Println("sent " + filepath.Base(img.Path))
	return 0
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

func truncatePrompt(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
