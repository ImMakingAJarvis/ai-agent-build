package main

// rollback.go: jarvis-kq rollback <queue> — the plan §7 rollback step for
// one queue. AMQP cannot delete a message by id, so the purge works from
// the mirror boundary: kshadow's state (LastID, updated per message) names
// the last message the mirror has captured. The queue preserves publish
// order and the mirror mirrors in the same order, so:
//
//   - popping the boundary message confirms every message popped before it
//     was mirrored (the mirror had already passed them) → drop all of them;
//     everything left in the queue came after the boundary → keep it.
//   - if the queue drains first, the boundary is not in it: every message
//     in it was published after the mirror's last write (kshadow lagged or
//     died) → requeue all of them to their original exchange and report.
//
// --dry-run pops with requeue, which puts the message back at the head of
// the queue: one pop is all it can show, so it classifies the oldest
// message against the boundary and reports (the queue is left unchanged).
//
// Exit 0 = queue clean for the cutover; 3 = undetermined messages
// requeued (verify them first).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kq"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/kshadow"
)

// watermarkRec mirrors kshadow's kq.watermark record (it is unexported
// there; the fields are the wire format both sides agree on).
type watermarkRec struct {
	Queue     string `json:"queue"`
	Exchange  string `json:"exchange"`
	MessageID string `json:"message_id"`
	Seq       int64  `json:"seq"`
}

// boundaryID resolves the last mirrored message id for the queue: kshadow's
// state file first (per-message, freshest), then the newest kq.watermark
// record for the queue (the throttled log).
func boundaryID(ctx context.Context, c *kq.Client, statePath, queue string) (string, string, error) {
	st, err := kshadow.LoadState(statePath)
	if err == nil {
		if q, ok := st.Queues[queue]; ok && q.LastID != "" {
			return q.LastID, "kshadow state (last_id)", nil
		}
	}
	last, err := c.LastOffsets(ctx, kshadow.WatermarkTopic)
	if err != nil {
		return "", "", nil // watermark topic never written
	}
	from := last[0] - 500
	if from < 0 {
		from = 0
	}
	msgs, err := c.ReadRange(ctx, kshadow.WatermarkTopic, 0, from, last[0], 500)
	if err != nil {
		return "", "", nil
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		var wm watermarkRec
		if err := json.Unmarshal(msgs[i].Value, &wm); err == nil && wm.Queue == queue && wm.MessageID != "" {
			return wm.MessageID, "kq.watermark topic", nil
		}
	}
	return "", "", nil
}

// cmdRollback: rollback <queue> [--dry-run] [--max 10000]
func cmdRollback(ctx context.Context, c *kq.Client, args []string) int {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "classify without dropping (messages go back)")
	max := fs.Int("max", 10000, "safety cap on popped messages")
	statePath := fs.String("state", envOr("KSHADOW_STATE", "/home/agent/.local/share/jarvis/kshadow/kshadow-state.json"), "kshadow's state file")
	amqpURL := fs.String("amqp", envOr("RABBITMQ_URL", "amqp://jarvis:jarvis-local@localhost:5672"), "RabbitMQ URL")
	pos := positional(args)
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: rollback <queue> [--dry-run] [--max 10000]")
		return 2
	}
	queue := pos[0]
	if err := fs.Parse(flagsAfterPositional(args)); err != nil {
		return 2
	}

	id, source, err := boundaryID(ctx, c, *statePath, queue)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-kq: boundary: %v\n", err)
		return 1
	}
	if id == "" {
		fmt.Fprintf(os.Stderr, "jarvis-kq: no mirror boundary for %s (kshadow state and kq.watermark both empty) — refusing to touch the queue\n", queue)
		return 1
	}
	admin, err := kshadow.NewAdmin(*amqpURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-kq: %v\n", err)
		return 1
	}
	ackmode := "reject_requeue_false"
	if *dryRun {
		ackmode = "reject_requeue_true"
	}

	fmt.Printf("queue %s: boundary %s (%s), popping (ackmode %s)\n", queue, id, source, ackmode)
	var popped []*kshadow.QueueMessage
	found := false
	for i := 0; i < *max; i++ {
		msg, err := admin.GetQueueMessage(ctx, queue, ackmode)
		if err != nil {
			fmt.Fprintf(os.Stderr, "jarvis-kq: pop: %v\n", err)
			return 1
		}
		if msg == nil {
			break // drained without the boundary: none of it was mirrored
		}
		popped = append(popped, msg)
		if msg.MessageID() == id {
			found = true
			break
		}
		// requeue_true puts the message back at the HEAD of the queue, so
		// the dry-run loop would re-pop the same oldest message forever:
		// one pop is all the dry run can see.
		if *dryRun {
			break
		}
	}
	if len(popped) == 0 {
		fmt.Printf("queue %s: empty — nothing to roll back\n", queue)
		return 0
	}

	// undetermined: the boundary was not popped, so nothing was confirmed
	// mirrored — republish everything to its original exchange.
	if !found && !*dryRun {
		bad := 0
		for _, m := range popped {
			// m.Payload is the decoded body (GetQueueMessage decodes it);
			// this mgmt build's publish endpoint only takes base64.
			payload := base64.StdEncoding.EncodeToString([]byte(m.Payload))
			if err := admin.PublishToExchange(ctx, m.Exchange, m.RoutingKey, m.Properties, payload, "base64"); err != nil {
				fmt.Fprintf(os.Stderr, "jarvis-kq: requeue (exchange %s, key %s): %v\n", m.Exchange, m.RoutingKey, err)
				bad++
			}
		}
		fmt.Printf("queue %s: boundary %s not in the queue — %d message(s) undetermined, requeued (%d failed)\n", queue, id, len(popped), bad)
		fmt.Println("kshadow appears to be lagging or dead — verify the requeued messages and kshadow's health before the cutover")
		return 3
	}

	if *dryRun {
		// the popped message went back (requeue_true) — the queue is
		// unchanged. message_count is the depth at pop time (before the
		// requeue), so +1 is the current depth; the broker's
		// messages_ready lags by a couple of seconds and is not trusted
		// here.
		depth := int(popped[0].MessageCount) + 1
		if found {
			fmt.Printf("queue %s (dry run): the oldest message IS the boundary %s — all %d message(s) are mirrored; the rollback would drop all of them, keeping none\n",
				queue, id, depth)
		} else {
			fmt.Printf("queue %s (dry run): the oldest message %s is not the boundary %s — it is deeper in the queue or absent (kshadow lagged); the rollback pops until it reaches the boundary (drop all popped) or runs dry (requeue all, exit 3)\n",
				queue, popped[0].MessageID(), id)
		}
		return 0
	}
	fmt.Printf("queue %s: dropped %d message(s) up to and including the boundary; the queue is clean for the cutover\n", queue, len(popped))
	return 0
}
