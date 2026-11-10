package agentjob

// fixnow.go: context handoff for the !fixnow preemption.
//
// A !fixnow (jarvis-chat) interrupts the running round — the same
// restart-signal mechanism !restart and !photonow use — and enqueues the
// requested fix at the top of the fix band. The interrupted job is put back
// to ready and runs again after the fix. Unlike a plain !restart, a !fixnow
// cut-off saves the interrupted round's context to Redis so the re-run picks
// up where it left off instead of starting cold: the round that is killed by
// the preemption writes a FixNowContext (job metadata + the tail of its
// transcript) under a per-job key with a TTL, and the next round for that
// job reads it back into its prompt (fixNowResumeBlock, called from Prompt).
//
// The TTL is the "so it doesn't pile up" guard: an interrupted job that is
// never re-run (deprioritized, skipped, the machine reboots and moves on)
// leaves its context in Redis for at most FixNowTTL, then it expires. The
// check at the start of every round always happens and simply returns nothing
// for a novel job (one that was never cut off by a !fixnow).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// FixNowSignal is the content jarvis-chat writes to the restart-signal file
// when a !fixnow preempts the running round. signalWatcher captures it before
// consuming the file so the killed round knows it was a !fixnow (and should
// save its context) rather than a plain !restart (a deliberate fresh start).
const FixNowSignal = "!fixnow"

// FixNowTTL is how long an interrupted round's saved context survives in
// Redis: long enough to cover the interrupting fix plus the re-run of the
// interrupted job, short enough that an interrupted job that is never picked
// up again does not pile up.
const FixNowTTL = 24 * time.Hour

// fixNowCtxKey is the Redis key holding one job's saved (preempted) context.
func fixNowCtxKey(jobID string) string { return "jarvis:fixnow:ctx:" + jobID }

// fixNowCtxMax caps the transcript stored in Redis so a pathological round
// can never pin an unbounded blob (the full durable copy stays in
// transcript.txt either way).
const fixNowCtxMax = 2 * 1024 * 1024

// fixNowPromptTail bounds how much of the stored transcript the resume block
// injects into the round prompt: the most recent work is what the resume
// needs, and the full record is one Read away in the job directory.
const fixNowPromptTail = 16 * 1024

// FixNowPreemptedFile is the job-dir file holding the full transcript of the
// round cut off by a !fixnow. It is written separately from transcript.txt
// (which the re-run truncates at the start of the new round) so the resume
// block can point the model at the interrupted round's output without it being
// clobbered. A later preemption of the same job overwrites it with a fresh
// record, which is correct — it always holds the most recent cut-off round.
const FixNowPreemptedFile = "transcript.preempted.txt"

// FixNowContext is the machine-readable record of a round that was
// interrupted by a !fixnow preemption. It is saved to Redis (per-job key,
// FixNowTTL) and read back into the re-run's prompt so the job resumes
// instead of restarting.
type FixNowContext struct {
	JobID   string `json:"job_id"`
	Title   string `json:"title"`
	Driver  string `json:"driver,omitempty"`
	Model   string `json:"model,omitempty"`
	Round   string `json:"round,omitempty"`
	SavedAt string `json:"saved_at"`
	// Transcript is the interrupted round's output, bounded to fixNowCtxMax
	// (its tail). The re-run's resume block shows its own tail of this and
	// points at transcript.txt for the rest.
	Transcript string `json:"transcript,omitempty"`
}

// fixNowRedis dials Redis (the same address job output is live-streamed to)
// with a short timeout. It returns nil when Redis is disabled or
// unreachable — a dead Redis must never block or fail a round, so every
// caller treats nil as "fixnow context unavailable" and carries on.
func fixNowRedis() *redis.Client {
	addr := StreamAddr()
	if addr == "off" || addr == "none" {
		return nil
	}
	opt, err := redis.ParseURL(addr)
	if err != nil {
		return nil
	}
	if opt.DialTimeout == 0 {
		opt.DialTimeout = time.Second
	}
	c := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		c.Close()
		return nil
	}
	return c
}

// SaveFixNowContext stores an interrupted round's context under the job's
// key, expiring in FixNowTTL. It is a best-effort no-op (nil) when the
// context is empty or Redis is unreachable — losing the handoff only means
// the re-run starts cold, which is the pre-fixnow behaviour.
func SaveFixNowContext(fc *FixNowContext) error {
	if fc == nil || fc.JobID == "" {
		return nil
	}
	c := fixNowRedis()
	if c == nil {
		return nil
	}
	defer c.Close()
	b, err := json.Marshal(fc)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Set(ctx, fixNowCtxKey(fc.JobID), b, FixNowTTL).Err()
}

// TakeFixNowContext loads and consumes (deletes) a job's saved context,
// returning nil when there is none — a novel job, an expired context, or a
// dead Redis. The delete is the consume: the re-run uses the context once,
// and a later preemption of the same job stores a fresh record.
func TakeFixNowContext(jobID string) *FixNowContext {
	if jobID == "" {
		return nil
	}
	c := fixNowRedis()
	if c == nil {
		return nil
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	b, err := c.Get(ctx, fixNowCtxKey(jobID)).Bytes()
	if err != nil {
		return nil
	}
	_ = c.Del(ctx, fixNowCtxKey(jobID)).Err() // consume; do not re-inject next round
	var fc FixNowContext
	if err := json.Unmarshal(b, &fc); err != nil {
		return nil
	}
	return &fc
}

// fixNowResumeBlock renders the resume context for a round picking up after a
// !fixnow preemption. It marks the round as a resume (not a fresh start),
// shows the job and what it was working on (the tail of the interrupted
// transcript), and points at the full record. It returns "" when there is no
// saved context, so an ordinary round is prompt-identical to before.
func fixNowResumeBlock(fc *FixNowContext) string {
	if fc == nil {
		return ""
	}
	tail := fc.Transcript
	if len(tail) > fixNowPromptTail {
		tail = "…" + tail[len(tail)-fixNowPromptTail:]
	}
	var b strings.Builder
	b.WriteString("\n=== RESUME — this round was cut off by a !fixnow preemption; pick the work back up where it stopped, do not start over ===\n")
	fmt.Fprintf(&b, "Job: %s — %s\n", fc.JobID, fc.Title)
	if fc.Model != "" {
		fmt.Fprintf(&b, "Interrupted while on: %s (driver %s, round %s)\n", fc.Model, fc.Driver, fc.Round)
	}
	if fc.SavedAt != "" {
		fmt.Fprintf(&b, "Cut off at: %s\n", fc.SavedAt)
	}
	b.WriteString("\nWhat you were doing (tail of the interrupted round):\n")
	b.WriteString(tail)
	fmt.Fprintf(&b, "\n\nThe full transcript of the interrupted round is in your job directory (%s). Read more of it if the tail above is not enough, then continue from where you left off — do not redo work that was already done.\n", FixNowPreemptedFile)
	b.WriteString("=== END RESUME ===\n")
	return b.String()
}

// boundTranscript returns the last fixNowCtxMax bytes of a transcript so the
// Redis record is bounded no matter how long the round ran.
func boundTranscript(s string) string {
	if len(s) <= fixNowCtxMax {
		return s
	}
	return s[len(s)-fixNowCtxMax:]
}
