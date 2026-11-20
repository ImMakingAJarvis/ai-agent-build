// The owner's direct line to the iGPU worker: `!igpu <task>` queues the
// task as an agent job pinned to the igpu driver — a round on the small
// always-on model on the Ryzen iGPU (jarvis-engine-igpu, port 11435;
// docs/local-brain.md, "The igpu driver").
//
// It exists for recovery. When the 4090 brain is down — resourced holds
// the GPU for a game (rightly or wrongly), a swap window is stuck, the
// engine crashed — nothing that needs the brain can run: `!build` / `!fix`
// jobs are driver-local and `jarvis-jobs next` skips them while the engine
// is down or the game-hold marker is set, and the chat brain cannot
// classify an "enqueue" request. An igpu job depends on none of that:
//
//   - the command is parsed here, with no model call;
//   - the job goes straight into the agentjob store (no RabbitMQ hop, the
//     same path as !build / !fix), with driver igpu in its spec;
//   - `jarvis-jobs next` gates an igpu job only on the iGPU engine's
//     /health — never on the brain or the game-hold marker — and
//     jarvis-swap's round guard and resourced's window rules count only
//     driver-local rounds.
//
// The job lands at the top of the fix band so it is the next thing the
// agent-run loop pulls, and carries the igpu-direct tag so the supervised
// loop runs it on the iGPU model without asking who should (full auto
// honors the spec driver by itself).
//
// Like every daemon command it is owner-only by construction: the chat
// layer fires the OnMessage hook only for the owner's DM
// (internal/chat/discord.go, DISCORD_OWNER_ID), so no other user's message
// can reach handleIGPU.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/chat"
)

// igpuJobPriority puts an !igpu job at the top of the fix band, level with
// !fixnow and !photonow: when the owner reaches for the iGPU worker the
// rest of the queue is usually stuck behind a dead brain, and this is the
// job that unsticks it.
const igpuJobPriority = 1000

// igpuHealthTimeout bounds the iGPU engine probe in the confirmation.
var igpuHealthTimeout = 2 * time.Second

// cutIGPU recognizes the !igpu command and returns the task text after it.
// The command word is case-insensitive ("!iGPU" is how the word is usually
// written) and needs a word boundary — a space, a tab, a newline or the end
// of the message — so "!igpus are slow" stays an ordinary chat message.
func cutIGPU(content string) (task string, ok bool) {
	const cmd = "!igpu"
	if len(content) < len(cmd) || !strings.EqualFold(content[:len(cmd)], cmd) {
		return "", false
	}
	rest := content[len(cmd):]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' && rest[0] != '\n' && rest[0] != '\r' {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// igpuTitle is the job title: the first line of the task, cut to 80 bytes
// on a rune boundary, behind an "igpu:" label so the job is recognizable
// in the queue and at the checkpoint.
func igpuTitle(task string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(task), "\n")
	first = strings.TrimSpace(first)
	if len(first) > 80 {
		cut := 80
		for cut > 0 && !utf8.RuneStart(first[cut]) {
			cut--
		}
		first = strings.TrimSpace(first[:cut])
	}
	return "igpu: " + first
}

// igpuJobInstructions wraps the owner's task for the small model. The
// stage-3 igpu brief is written for coding rounds ("do not try to restart
// the brain", "finish with a pushed branch and a PR"); an !igpu task is an
// operations task where restarting the brain may be the whole point, so
// the wrapper says which of those rules give way, keeps the model on the
// literal task, and fixes how the outcome is reported.
func igpuJobInstructions(id, task string) string {
	return `OWNER TASK FOR THE iGPU WORKER (sent with the !igpu Discord command).

I sent this to you — the small model on the iGPU — on purpose. Usually that
means the 4090 brain is down, held for a game, or stuck, and you are the
part of the system that still works. The brain being down is expected.

Rules for this job (where they differ from the general rules above, these win):
- Do exactly what the TASK below says, step by step, with shell commands.
  Do not pick other work and do not add steps of your own.
- This is an operations task, not a coding job: no branch, no commit and
  no PR, unless the task itself asks for one.
- If the task tells you to start, stop or restart the brain or another
  service, do it — that is the point of this job. Never do it on your own
  initiative.
- Run each command once and read its output before the next one. If a
  command fails, try it at most twice; do not improvise a different fix.
- Finish with exactly ONE Discord message, then end the round:
    done:    jarvis-discord notify "igpu job ` + id + ` done: <what you ran and the result, one or two lines>"
    failed:  jarvis-discord notify "igpu job ` + id + ` FAILED: <the command that failed and the last line of its output>"

TASK:
` + strings.TrimSpace(task) + "\n"
}

// enqueueIGPUJob stores the task as an agent job routed to the iGPU
// worker: driver igpu in the spec, class fix at igpuJobPriority, tagged
// igpu-direct.
func enqueueIGPUJob(in chat.Incoming, task string) (id string, err error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return "", fmt.Errorf("empty task")
	}
	store, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		return "", err
	}
	id = newChatJobID()
	_, err = store.Ingest(agentjob.Spec{
		ID:               id,
		Title:            igpuTitle(task),
		Instructions:     igpuJobInstructions(id, task),
		Driver:           "igpu",
		Class:            agentjob.ClassFix,
		Priority:         igpuJobPriority,
		Source:           "discord",
		DiscordChannelID: in.ChannelID,
		DiscordMessageID: in.MessageID,
		Tags:             []string{agentjob.TagIGPUDirect},
	})
	return id, err
}

// igpuEngineURL is where this daemon reaches the iGPU engine: IGPU_URL
// when set, else FALLBACK_ENGINE_URL (docker-compose points the chat
// container's fallback at the iGPU helper). Empty when neither is set.
func igpuEngineURL() string {
	if u := os.Getenv("IGPU_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return strings.TrimRight(os.Getenv("FALLBACK_ENGINE_URL"), "/")
}

// igpuEngineState probes the iGPU engine's /health. known is false when
// this daemon has no URL for the engine, so the confirmation says nothing
// about it rather than guessing. Only a 200 counts as up — the same rule
// `jarvis-jobs next` applies before it offers an igpu job.
func igpuEngineState() (up, known bool) {
	base := igpuEngineURL()
	if base == "" {
		return false, false
	}
	client := &http.Client{Timeout: igpuHealthTimeout}
	resp, err := client.Get(base + "/health")
	if err != nil {
		return false, true
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK, true
}

// igpuConfirm is the owner reply to an !igpu: the job id, where the job
// went, and anything that will keep it from running soon (the loop is
// stopped, or the iGPU engine itself is not answering).
func igpuConfirm(id string, loopUp, engineUp, engineKnown bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Queued iGPU job %s: top of the fix band, pinned to the igpu driver (the small model on the iGPU). It does not need the 4090 brain, and a game hold does not block it.", id)
	if loopUp {
		b.WriteString(" agent-run is up; it runs next — after the current round, if one is running.")
	} else {
		b.WriteString(" agent-run is not running, so it will wait — !restart-service starts the loop.")
	}
	if engineKnown && !engineUp {
		b.WriteString(" Warning: the iGPU engine is not answering /health right now; the job stays queued until it does.")
	}
	b.WriteString(" The round reports the result here, or a short failure note.")
	return b.String()
}

// handleIGPU runs the !igpu command and reports whether the message was
// one. A bare "!igpu" is still the command (the owner clearly meant it),
// so it answers with usage instead of falling through to the brain.
func handleIGPU(d sender, in chat.Incoming) bool {
	task, ok := cutIGPU(strings.TrimSpace(in.Content))
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if task == "" {
		_ = d.Notify(ctx, "Need a task after !igpu — e.g. !igpu run `jarvis-swap status` and tell me what it prints")
		return true
	}
	id, err := enqueueIGPUJob(in, task)
	if err != nil {
		log.Printf("jarvis-chat: !igpu enqueue: %v", err)
		_ = d.Notify(ctx, "Could not enqueue the iGPU job: "+err.Error())
		return true
	}
	engineUp, engineKnown := igpuEngineState()
	_ = d.Notify(ctx, igpuConfirm(id, agentRunUp(), engineUp, engineKnown))
	return true
}
