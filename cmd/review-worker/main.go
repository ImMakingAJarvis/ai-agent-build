// Command review-worker is the cloud PR review worker (Job 6 of
// docs/pr-review-build-plan.md). It consumes one review queue
// (review.sonnet, review.opus or review.fable) with prefetch 1 and manual ack,
// and reviews each pull request in a throwaway worktree: it runs the Go gate,
// then the cloud reviewer (the claude binary) if the gate passes.
//
// Usage:
//
//	review-worker -queue review.sonnet -model sonnet
//
// The -queue and -model flags are required. review.local is refused (a
// container must never load the host's 27B); local reviews run in the existing
// agent-run loop. Optional flags:
//
//	-repo <dir>       the git repo the worktrees are created from (default .)
//	-base <dir>       where worktrees are created (default under os.TempDir)
//	-state-dir <dir>  the reviewstate dir (default reviewstate.DefaultDir)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// defaultBaseDir is where worktrees are created when -base is not given: a
// per-process temp dir so worktrees never touch the live checkout.
func defaultBaseDir() string {
	if d := os.Getenv("REVIEW_WORKTREE_BASE"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "review-worker")
}

func main() {
	queueName := flag.String("queue", "", "the review queue to consume (required)")
	model := flag.String("model", "", "the reviewer: sonnet, opus, or fable (required)")
	repo := flag.String("repo", os.Getenv("REVIEW_REPO"), "the git repo to create worktrees from (default .)")
	base := flag.String("base", os.Getenv("REVIEW_WORKTREE_BASE"), "where worktrees are created (default a temp dir)")
	stateDir := flag.String("state-dir", reviewstate.DefaultDir(), "the reviewstate dir (held check)")
	flag.Parse()

	w := &Worker{
		Queue:   *queueName,
		Model:   *model,
		RepoDir: *repo,
		BaseDir: *base,
		Dir:     *stateDir,
	}
	if err := w.Setup(); err != nil {
		log.Fatalf("review-worker: %v", err)
	}
	// Defaults run the real commands; tests substitute fakes.
	w.Load = reviewstate.Load
	w.Worktree = w.makeWorktree
	w.Gate = w.defaultGate
	w.Claude = w.runClaude

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, w); err != nil && ctx.Err() == nil {
		log.Fatalf("review-worker: %v", err)
	}
}

// run connects to the broker and consumes w.Queue, riding out broker restarts:
// on any non-shutdown error it reconnects and re-consumes.
func run(ctx context.Context, w *Worker) error {
	for {
		if err := consumeOnce(ctx, w); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("review-worker: %v; reconnecting in 5s", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// consumeOnce opens one channel and consumes w.Queue until ctx is done or the
// delivery channel closes (the broker connection dropped — run reconnects).
func consumeOnce(ctx context.Context, w *Worker) error {
	conn, err := queue.NewConn()
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()
	ch, err := conn.NewChannel()
	if err != nil {
		return fmt.Errorf("channel: %w", err)
	}
	defer ch.Close()
	// The review queues are durable with no arguments (rabbitmq/definitions.json);
	// declare idempotently so a fresh broker still has them.
	if _, err := ch.QueueDeclare(w.Queue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare %s: %w", w.Queue, err)
	}
	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("qos: %w", err)
	}
	log.Printf("review-worker: consuming %s as %s (prefetch=1, manual ack)", w.Queue, w.Model)
	return w.Run(ctx, &brokerSource{ch: ch, q: w.Queue, ctx: ctx})
}

// brokerSource is the production Source: it wraps an AMQP channel and delivers
// the queue's messages one at a time (prefetch 1, manual ack) to the handler.
type brokerSource struct {
	ch  *amqp.Channel
	q   string
	ctx context.Context
}

func (b *brokerSource) Consume(handler func(Delivery)) error {
	dels, err := b.ch.Consume(b.q, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume %s: %w", b.q, err)
	}
	for {
		select {
		case <-b.ctx.Done():
			return b.ctx.Err()
		case del, ok := <-dels:
			if !ok {
				return fmt.Errorf("delivery channel closed for %s (broker connection dropped)", b.q)
			}
			handler(Delivery{
				Body: del.Body,
				Ack:  func() { _ = del.Ack(false) },
				Nack: func() { _ = del.Nack(false, false) }, // do not requeue
			})
		}
	}
}
