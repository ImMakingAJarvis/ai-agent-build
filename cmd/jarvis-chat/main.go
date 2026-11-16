// jarvis-chat is an always-on daemon that listens for owner DMs on Discord
// and replies using the local Ollama brain. It runs alongside the build
// round and does not interrupt it.
//
// On each owner DM it:
//  1. Adds a 👀 reaction and starts typing (refreshed every 8 s).
//  2. Calls the brain with a short system prompt, status line, and
//     conversation history (last 10 turns, ≤3000 words).
//  3. Replies with Notify.
//  4. If no answer after 20 s, sends a busy notice.
//  5. Gives up after 4 minutes with a timeout message.
//  6. Appends the exchange to the daily conversation log
//     (notes/conversations/discord-YYYY-MM-DD.md).
//
// It skips messages while a checkpoint question is open
// ($CONF/ask-open exists) and skips Discord replies to bot questions
// (those belong to Ask).
//
// Multi-bot support:
// The primary bot (DISCORD_BOT_TOKEN) uses the basic chat handler.
// Additional bots (BOT_N_TOKEN) use the Hermes runner, which can
// search the web, enqueue jobs, and delegate complex tasks.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"sync"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/art"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/chat"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/config"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/hermes"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/memory"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photomind"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/voices"
)

const (
	// Typing is refreshed every 8 seconds to keep the indicator alive.
	typingInterval = 8 * time.Second

	// Maximum conversation history in words.
	maxHistoryWords = 3000

	// Maximum number of recent turns to include.
	maxTurns = 10

	// Default model for chat.
	defaultModel = "jarvis-brain"

	// Default voice key.
	defaultVoice = "jarvis"
)

// Timeouts are variables so tests can shorten them.
var (
	busyTimeout = 20 * time.Second // send busy notice after this long
	maxTimeout  = 4 * time.Minute  // give up after this long
	// photoCheckTimeout covers the photocheck loop: two generations plus
	// two thinking-mind calls.
	photoCheckTimeout = 6 * time.Minute
)

// sender abstracts the Discord operations handleMessage needs,
// so tests can substitute a fake.
type sender interface {
	Notify(context.Context, string) error
	Typing() error
}

// photoSender is what handlePhoto needs from the Discord side.
type photoSender interface {
	sender
	SendImage(context.Context, string, string) error
}

func main() {
	loadEnvFile()

	cfg, err := config.Load()
	if err != nil {
		log.Printf("jarvis-chat: config load warning: %v", err)
	}

	confDir := os.Getenv("JARVIS_CONF")
	if confDir == "" {
		home, _ := os.UserHomeDir()
		confDir = filepath.Join(home, ".config", "jarvis")
	}

	// If no bots configured via the config package, fall back to env vars.
	if cfg == nil || len(cfg.Bots) == 0 {
		primaryToken := os.Getenv("DISCORD_BOT_TOKEN")
		if primaryToken != "" {
			cfg = &config.Config{Bots: []config.BotConfig{
				{
					Name:         "primary",
					Token:        primaryToken,
					AllowedUsers: []string{os.Getenv("DISCORD_OWNER_ID")},
					AccessLevel:  config.AccessFull,
				},
			}}
		}
	}

	if cfg == nil || len(cfg.Bots) == 0 {
		log.Fatal("jarvis-chat: no Discord bot configured (set DISCORD_BOT_TOKEN or BOT_1_TOKEN)")
	}

	// While the worker is deep in a job the brain is saturated, so
	// classify/chat calls route to the small fallback engine
	// (FALLBACK_ENGINE_URL, the iGPU helper); NewFallbackClient is a
	// pass-through when that is unset.
	ollamaClient := NewFallbackClient(models.NewClient())
	modelName := os.Getenv("LOCAL_MODEL")
	if modelName == "" {
		modelName = defaultModel
	}

	// Select voice (personality) from config.
	voiceKey := os.Getenv("BOT_N_VOICE")
	if voiceKey == "" {
		voiceKey = defaultVoice
	}
	voice := voices.Get(voiceKey)
	log.Printf("jarvis-chat: using voice %q (%s)", voiceKey, voice.Name)

	// Create the Hermes agent for intent classification and action execution.
	hermesAgent := hermes.New(hermes.Config{
		Model:       modelName,
		MaxHistory:  20,
		AccessLevel: config.AccessFull,
		ChatSlot:    chatSlotID(),
	})
	hermesAgent.SetOllamaClient(ollamaClient)

	// Register default actions.
	for _, action := range hermes.DefaultActions() {
		hermesAgent.Register(action)
	}

	// Memory service: vector search over work logs, job history, queue.
	// MEMORY_URL is set by docker-compose (http://memory:8101); the
	// client fails open when the service is down.
	memClient := memory.NewClient(os.Getenv("MEMORY_URL"))
	hermesAgent.Register(hermes.MemoryAction(memClient))

	// Art service: local photo generation for the !photo command.
	// ART_URL is set by docker-compose (http://art:8120).
	photoGen := art.New()

	// Photo requests go through the thinking mind first: it expands the
	// owner's casual words into a detailed prompt for the image model
	// (and, for !photocheck, iterates on it).
	photoSvc := photomind.New(ollamaClient, modelName, photoGen)

	// Conversation history: alternating user/assistant turns.
	var history []string // [user1, assistant1, user2, assistant2, ...]

	// Start each bot in a goroutine.
	bots := make([]*chat.Discord, 0, len(cfg.Bots))
	for i, botCfg := range cfg.Bots {
		botIndex := i
		bCfg := botCfg // capture for closure

		d, err := chat.NewDiscord(chat.Config{
			Token:   bCfg.Token,
			OwnerID: pickOwnerID(bCfg, confDir),
		})
		if err != nil {
			log.Printf("jarvis-chat: bot %q: %v", bCfg.Name, err)
			continue
		}
		bots = append(bots, d)

		if botIndex == 0 {
			// Primary bot: basic chat handler.
			d.OnMessage(func(in chat.Incoming) {
				// Skip while a checkpoint question is open.
				if _, err := os.Stat(confDir + "/ask-open"); err == nil {
					return
				}

				content := strings.TrimSpace(in.Content)
				if content == "" {
					return
				}
				in.Content = content

				// !pause is a special command: write the pause-signal file so
				// agent-run can interrupt the current job, even when jarvis-chat
				// is active (and the Discord watcher is not running).
				if content == "!pause" {
					_ = os.WriteFile(confDir+"/pause-signal", []byte("!pause"), 0o644)
					return
				}

				// !restart is a special command: write the restart-signal file
				// so the running job's signal watcher kills the round and
				// re-queues the job — the agent run restarts on a fresh
				// conversation. Only useful while a round is running; between
				// rounds the file is cleared before the next round starts.
				if content == "!restart" {
					_ = os.WriteFile(confDir+"/restart-signal", []byte("!restart"), 0o644)
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					_ = d.Notify(ctx, "Restarting the agent run: killing the current round and putting its job back in the queue.")
					cancel()
					return
				}

				// !restart-service restarts the agent-run *service* itself,
				// not just the current round: it runs the agent-run-restart
				// helper, which defers the restart until the running round
				// ends (or takes it now with "now"), and starts the service
				// when it is down.
				if content == "!restart-service" || content == "!restart-service now" {
					var helperArgs []string
					if content == "!restart-service now" {
						helperArgs = append(helperArgs, "--now")
					}
					hctx, hcancel := context.WithTimeout(context.Background(), 60*time.Second)
					out, herr := exec.CommandContext(hctx, "/usr/local/bin/agent-run-restart", helperArgs...).CombinedOutput()
					hcancel()
					msg := strings.TrimSpace(string(out))
					if herr != nil {
						msg = "agent-run restart failed: " + msg
					}
					if msg == "" {
						msg = "agent-run restart failed with no output"
					}
					nctx, ncancel := context.WithTimeout(context.Background(), 15*time.Second)
					_ = d.Notify(nctx, msg)
					ncancel()
					return
				}

				// !automode on / off sets or clears the NoBrakes auto_mode
				// flag in Redis (owner-only: this hook fires only for the
				// owner DM, same as !pause / !restart).
				if _, _, isAutoMode := cutAutoMode(content); isAutoMode {
					handleAutoMode(d, agentjob.StreamAddr(), content)
					return
				}

				// !fullauto on / off sets or clears the NoBrakes full-auto
				// flag in Redis: when on, the agent-run loop runs the next
				// ready job immediately at any time of day (owner-only, same
				// as !automode).
				if _, _, isFullAuto := cutFullAuto(content); isFullAuto {
					handleFullAuto(d, agentjob.StreamAddr(), content)
					return
				}

				// !keepgoing on / off sets or clears the NoBrakes keep-going
				// flag in Redis: when on, a failed auto job notifies the
				// owner without pausing autonomous execution (owner-only,
				// same as !automode / !fullauto).
				if _, _, isKeepGoing := cutKeepGoing(content); isKeepGoing {
					handleKeepGoing(d, agentjob.StreamAddr(), content)
					return
				}

				// !photo / !photoassist / !photocheck generate a photo
				// with the local art model (prompt expanded by the
				// thinking mind) and send it back as an attachment.
				if mode, size, prompt, ok := cutPhotoCmd(content); ok {
					handlePhoto(d, photoSvc, in, confDir, mode, size, prompt)
					return
				}

				if kind, rest, ok := cutBuildFix(content); ok {
					title, body := titleAndBody(rest)
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					if title == "" {
						_ = d.Notify(ctx, "Need a title after !build / !fix.")
						return
					}
					if kind == "fixnow" {
						// Stop whatever is running and do this fix first: queue
						// it at the top of the fix band, then write the
						// !fixnow restart signal so the running round is cut off
						// and its context saved for the re-run (fixnow.go).
						id, err := enqueueFromChat(in, agentjob.ClassFix, title, body, fixNowJobPriority)
						if err != nil {
							_ = d.Notify(ctx, "Could not enqueue: "+err.Error())
							return
						}
						preemptRound(confDir, agentjob.FixNowSignal)
						_ = d.Notify(ctx, fixNowConfirm(id))
						return
					}
					class := agentjob.ClassBuild
					if kind == "fix" {
						class = agentjob.ClassFix
					}
					id, err := enqueueFromChat(in, class, title, body, chatJobPriority)
					if err != nil {
						_ = d.Notify(ctx, "Could not enqueue: "+err.Error())
						return
					}
					_ = d.Notify(ctx, enqueueConfirm(id))
					return
				}

				handleMessage(d, hermesAgent, voice, memClient, in, &history)
			})
		} else {
			// Additional bots: Hermes runner with enhanced capabilities.
			runner := newHermesRunner(bCfg)
			runner.SetDiscord(d)
			d.OnMessage(runner.handleMessage)
			log.Printf("jarvis-chat: bot %q started with Hermes runner (access: %v)",
				bCfg.Name, bCfg.AccessLevel)
		}
	}

	if len(bots) == 0 {
		log.Fatal("jarvis-chat: no bots could be started")
	}

	// Capability replies (EVB-32): submit_capability Results land on the
	// Hermes-owned flow queue; this durable consumer turns each one into a
	// Discord reply on the primary bot (the same notifier the switchboard
	// uses). Best-effort: no broker = no replies, the chat itself is
	// unaffected. The queue is durable, so a Result that arrives while the
	// daemon is down is answered on the next start.
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	go func() {
		replyBot := bots[0]
		for {
			conn, err := queue.NewConn()
			if err != nil {
				log.Printf("jarvis-chat: capability replies: broker: %v (retrying in 30s)", err)
				select {
				case <-watchCtx.Done():
					return
				case <-time.After(30 * time.Second):
				}
				continue
			}
			err = hermes.WatchCapResults(watchCtx, conn, func(r queue.Result) {
				nctx, ncancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer ncancel()
				if err := replyBot.Notify(nctx, hermes.FormatCapResult(r)); err != nil {
					log.Printf("jarvis-chat: capability reply: %v", err)
				} else {
					log.Printf("jarvis-chat: capability reply sent (%s, corr %s)", r.Capability, r.CorrelationID)
				}
			}, log.Printf)
			conn.Close()
			if watchCtx.Err() != nil {
				return
			}
			log.Printf("jarvis-chat: capability replies: consumer stopped: %v (reconnecting in 5s)", err)
			time.Sleep(5 * time.Second)
		}
	}()

	// Open all gateway connections.
	for _, d := range bots {
		if err := d.Listen(); err != nil {
			log.Fatalf("jarvis-chat: listen: %v", err)
		}
	}

	log.Printf("jarvis-chat: %d bot(s) listening for messages", len(bots))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	select {
	case <-sig:
		log.Println("jarvis-chat: shutting down")
	case <-time.After(24 * time.Hour):
	}

	// Cleanup.
	cancelWatch()
	for _, d := range bots {
		d.Close()
	}
}

// pickOwnerID returns the owner ID for a bot. For the primary bot, it uses
// DISCORD_OWNER_ID. For additional bots, it uses the first allowed user.
// Falls back to reading from the signal file config.
// chatSlotID returns the llama.cpp slot reserved for chat traffic. Slot 0
// is the work slot (build rounds); chat replies run on slot 1 so both
// happen at once. Override with CHAT_SLOT_ID.
func chatSlotID() int {
	if v := os.Getenv("CHAT_SLOT_ID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

func pickOwnerID(bCfg config.BotConfig, confDir string) string {
	// Try the first allowed user.
	if len(bCfg.AllowedUsers) > 0 {
		return bCfg.AllowedUsers[0]
	}
	// Fall back to env var.
	if id := os.Getenv("DISCORD_OWNER_ID"); id != "" {
		return id
	}
	// Fall back to reading from config.
	if b, err := os.ReadFile(filepath.Join(confDir, "owner-id")); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

func handleMessage(d sender, hAgent *hermes.Agent, voice *voices.Voice, mem *memory.Client, in chat.Incoming, history *[]string) {
	content := in.Content
	ctx, cancel := context.WithTimeout(context.Background(), maxTimeout)
	defer cancel()

	// Add 👀 reaction — we can't get the message ID from the hook,
	// so we skip the reaction and rely on typing as acknowledgment.
	// (The reaction requires the incoming message ID, which the
	//  OnMessage hook doesn't receive. The typing indicator serves
	//  as immediate acknowledgment.)

	// Start typing indicator.
	typingCtx, typingCancel := context.WithCancel(context.Background())
	defer typingCancel()
	go typingLoop(typingCtx, d)

	// Busy notice state.
	busySent := false
	var job string
	if b, err := os.ReadFile(os.Getenv("JARVIS_CONF") + "/current-job"); err == nil {
		job = strings.TrimSpace(string(b))
	}
	if job == "" {
		job = "an unknown task"
	}

	// Step 1: Use Hermes to classify the message.
	resultCh := make(chan *hermes.Result, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := hAgent.Classify(ctx, content)
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- result
	}()

	var result *hermes.Result
	select {
	case result = <-resultCh:
	case err := <-errCh:
		log.Printf("jarvis-chat: classify error: %v", err)
		time.Sleep(2 * time.Second)
		typingCancel()
		_ = d.Notify(ctx, "Sorry, my brain stumbled; try again in a moment.")
		return
	case <-time.After(busyTimeout):
		_ = d.Notify(ctx, "Busy on "+job+", my answer is coming")
		busySent = true
		select {
		case result = <-resultCh:
		case err := <-errCh:
			log.Printf("jarvis-chat: classify error: %v", err)
			typingCancel()
			_ = d.Notify(ctx, "Sorry, the brain is tied up; ask me again in a bit")
			return
		case <-ctx.Done():
			typingCancel()
			_ = d.Notify(ctx, "Sorry, the brain is tied up; ask me again in a bit")
			return
		}
	case <-ctx.Done():
		typingCancel()
		if !busySent {
			_ = d.Notify(ctx, "Sorry, the brain is tied up; ask me again in a bit")
		}
		return
	}

	var answer string

	switch result.Type {
	case hermes.TypeAction:
		// Step 2a: Execute the action via Hermes.
		status, err := hAgent.Execute(ctx, result)
		if err != nil {
			log.Printf("jarvis-chat: execute error: %v", err)
			typingCancel()
			_ = d.Notify(ctx, "Sorry, that action failed; try again.")
			return
		}
		answer = status

	case hermes.TypeChat:
		// Step 2b: Conversational — route through Hermes with full
		// conversation history and voice system prompt.
		systemPrompt := voice.SystemPrompt
		if status := getStatusLine(); status != "" {
			systemPrompt += "\n\nCurrent status:\n" + status
		}

		// RAG: pull relevant work-log context from the memory service so
		// the chat can reference past jobs, decisions and the pending
		// queue. Fails open — no hits, no context, chat goes on.
		if hits := mem.Search(ctx, content, 4); len(hits) > 0 {
			systemPrompt += "\n\nRelevant context from the work logs " +
				"(memory service; use it when it fits, cite the source):\n" +
				memory.FormatContext(hits, 1200)
		}

		// When the owner Discord-replies to a message, hand the brain the
		// message being replied to so it knows what the reply refers to.
		// Only the chat sees it; the plain words still go to history and the
		// conversation log.
		chatMsg := withReplyContext(in)

		answerCh := make(chan string, 1)
		errCh2 := make(chan error, 1)
		go func() {
			a, err := hAgent.Chat(ctx, systemPrompt, *history, chatMsg)
			if err != nil {
				errCh2 <- err
				return
			}
			answerCh <- a
		}()

		select {
		case answer = <-answerCh:
		case err := <-errCh2:
			log.Printf("jarvis-chat: brain error: %v", err)
			typingCancel()
			_ = d.Notify(ctx, "Sorry, my brain stumbled; try again in a moment.")
			return
		case <-time.After(busyTimeout):
			if !busySent {
				_ = d.Notify(ctx, "Busy on "+job+", my answer is coming")
				busySent = true
			}
			select {
			case answer = <-answerCh:
			case err := <-errCh2:
				log.Printf("jarvis-chat: brain error: %v", err)
				typingCancel()
				_ = d.Notify(ctx, "Sorry, the brain is tied up; ask me again in a bit")
				return
			case <-ctx.Done():
				typingCancel()
				_ = d.Notify(ctx, "Sorry, the brain is tied up; ask me again in a bit")
				return
			}
		case <-ctx.Done():
			typingCancel()
			if !busySent {
				_ = d.Notify(ctx, "Sorry, the brain is tied up; ask me again in a bit")
			}
			return
		}

	default:
		log.Printf("jarvis-chat: unknown result type %q", result.Type)
		typingCancel()
		_ = d.Notify(ctx, "Sorry, I didn't understand that classification.")
		return
	}

	// Got an answer.
	typingCancel()

	// For chat replies, check for SCHEDULE blocks to enqueue jobs.
	if result.Type == hermes.TypeChat {
		visible, extra := applySchedule(answer, in)
		if extra != "" {
			if strings.TrimSpace(visible) != "" {
				answer = strings.TrimSpace(visible) + "\n\n" + extra
			} else {
				answer = extra
			}
		}
	}

	// Update history: append user message + assistant reply.
	*history = append(*history, content, answer)
	pruneHistory(history, maxHistoryWords)

	// Leave a written record of the exchange.
	logConversation(content, answer)

	if err := d.Notify(ctx, answer); err != nil {
		log.Printf("jarvis-chat: notify: %v", err)
	}
}

// handlePhoto routes a photo request. The plain, "assist" and "now" flavors
// are QUEUED: the prompt is expanded now (the brain is up), then a photo job
// is enqueued that generates and delivers the image in the brain-swap window,
// when the 4090 is free — so a full GPU no longer makes the request fail
// synchronously. "now" additionally interrupts the running job so the photo
// runs next (it is queued at the top of the fix band). "check" keeps its
// synchronous draft→generate→critique loop, which needs the brain resident
// and therefore cannot run in the swap window.
//
// A DM that carries an image attachment (png/jpeg/webp) pastes a photo with
// the prompt: the image is downloaded here, stored as the job's reference
// sidecar, and the round generates with the reference-capable model so the
// result keeps the subject (the owner) from the attached photo.
func handlePhoto(d photoSender, svc *photomind.Service, in chat.Incoming, confDir, mode, size, prompt string) {
	prompt = strings.TrimSpace(prompt)
	cmdName := "!photo"
	now := false
	switch mode {
	case "assist":
		cmdName = "!photoassist"
	case "assistnow":
		cmdName = "!photoassistnow"
		now = true
	case "check":
		cmdName = "!photocheck"
	case "now":
		cmdName = "!photonow"
		now = true
	}
	if prompt == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = d.Notify(ctx, "Need a prompt after "+cmdName+" — e.g. "+cmdName+" a photo of a red panda in fresh snow (optionally with a size first, "+cmdName+" 16x9 a red panda in fresh snow)")
		return
	}

	// "check" needs the brain resident (it critiques after each draft), so it
	// stays synchronous — the queued swap-window flow does not provide that.
	if mode == "check" {
		runPhotoCheck(d, svc, prompt, size)
		return
	}

	// Everything else is queued. Expand the prompt now, while the brain is up.
	typingCtx, typingCancel := context.WithCancel(context.Background())
	defer typingCancel()
	go typingLoop(typingCtx, d)

	// An attached image is the reference: the subject the generated photo
	// must keep. Fetch it now (the owner's photo, e.g. a pasted face shot)
	// and store it on the job; the round stages it and generates with the
	// reference-capable model.
	var refBytes []byte
	if in.AttachmentURL != "" {
		b, err := downloadAttachment(in.AttachmentURL)
		if err != nil {
			log.Printf("jarvis-chat: photo attachment download: %v", err)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			_ = d.Notify(ctx, "I couldn't download the image you attached — I've queued your photo without a reference.")
			cancel()
		} else if len(b) > 0 {
			refBytes = b
		}
	}

	usedPrompt := prompt
	note := ""
	if !svc.Healthy() {
		note = "thinking mind is down — I used your words as-is"
	} else if p, err := assistPrompt(svc, prompt, size); err == nil {
		usedPrompt = p
	} else {
		note = "the thinking mind was down — I used your words as-is"
		log.Printf("jarvis-chat: photo assist: %v", err)
	}

	if mode == "assist" || mode == "assistnow" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = d.Notify(ctx, "Here's the prompt I'll use:\n\n"+usedPrompt)
		cancel()
	}

	id, err := enqueuePhotoJob(in, prompt, usedPrompt, size, now, refBytes)
	if err != nil {
		log.Printf("jarvis-chat: photo enqueue: %v", err)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = d.Notify(ctx, "Could not queue your photo: "+err.Error())
		cancel()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	if now {
		// Interrupt the running job (if any) so this top-priority photo runs
		// next; the interrupted job is re-queued and resumes after the photo.
		preemptRound(confDir, "!photonow")
		_ = d.Notify(ctx, "Now! I've queued your photo at the very top ("+id+"). I'll pause the current job, generate it in the brain-swap window, send it to your DM, then pick the paused work back up.")
	} else {
		what := "your photo"
		if size != "" {
			what = "your " + size + " photo"
		}
		msg := "Queued " + what + " as " + id + ". It'll run in the brain-swap window — after any running fix work, ahead of ordinary build work — and I'll send it to your DM as soon as it's generated."
		if note != "" {
			msg = note + ". " + msg
		}
		_ = d.Notify(ctx, msg)
	}
	cancel()
}

// assistPrompt asks the thinking mind to expand a casual photo request into a
// detailed image prompt. It is isolated so handlePhoto can fall back to the
// owner's raw words on any brain hiccup.
func assistPrompt(svc *photomind.Service, request, size string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return svc.Assist(ctx, request, size)
}

// downloadAttachment fetches one Discord attachment URL (the owner's pasted
// image) and returns its bytes, so it can be stored as the photo job's
// reference sidecar. It caps the body: a Discord image attachment for this
// bot is well under 25 MB, and a reference photo is a few MB.
func downloadAttachment(u string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("attachment fetch: HTTP %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 25<<20))
}

// runPhotoCheck is the synchronous !photocheck loop: draft → generate →
// critique → improve → generate. It needs the brain resident (for the
// critique) and the 4090 free (for generation), so it stays synchronous
// rather than queued.
func runPhotoCheck(d photoSender, svc *photomind.Service, prompt, size string) {
	typingCtx, typingCancel := context.WithCancel(context.Background())
	defer typingCancel()
	go typingLoop(typingCtx, d)

	genCtx, genCancel := context.WithTimeout(context.Background(), photoCheckTimeout)
	defer genCancel()

	var (
		img        *art.Image
		usedPrompt = prompt
		note       string
		err        error
	)
	var res *photomind.CheckResult
	res, err = svc.Check(genCtx, prompt, size)
	if res != nil {
		img, usedPrompt = res.Image, res.Prompt
		if len(res.Drafts) > 1 {
			note = "photocheck: 2 passes — critiqued the first prompt, improved it, re-generated"
		}
	}
	genCancel()
	typingCancel()

	if img == nil {
		msg := "Photo generation returned nothing; try again in a moment."
		if err != nil {
			msg = photoErrorMessage(err)
		}
		log.Printf("jarvis-chat: photocheck: %v", err)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = d.Notify(ctx, msg)
		return
	}
	if err != nil {
		// A check loop that died after the first pass: show what we have.
		note += " — " + photoErrorMessage(err)
	}

	// Fresh context: the generation ate most of the timeout already.
	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer sendCancel()
	caption := photoCaption(img, usedPrompt, note)
	if err := d.SendImage(sendCtx, caption, img.Path); err != nil {
		log.Printf("jarvis-chat: photo send: %v", err)
		_ = d.Notify(sendCtx, "The photo is done ("+img.Path+") but I could not send the file: "+err.Error())
	}
}

// preemptRound interrupts the running job (if any) so the caller's
// top-priority work runs next, mirroring the !restart signal. cmd is written
// to the restart-signal file so the killed round knows why it was cut off
// ("!photonow", "!fixnow"); only a "!fixnow" cut-off saves the round's context
// for the re-run (agentjob). Between rounds it is a no-op: the loop clears the
// signal before each round and Store.Restart only acts on a running job, so a
// stale signal cannot interrupt the next job.
func preemptRound(confDir, cmd string) {
	if confDir == "" {
		return
	}
	_ = os.WriteFile(confDir+"/restart-signal", []byte(cmd), 0o644)
}

// photoCaption builds the caption under a generated photo: an optional
// note (brain fallback / check-loop outcome), the prompt that was
// actually sent to the image model (truncated; the full prompt is in
// the gallery), the generation stats, and the gallery link.
func photoCaption(img *art.Image, usedPrompt, note string) string {
	if len(usedPrompt) > 600 {
		usedPrompt = usedPrompt[:600] + "…"
	}
	caption := ""
	if note != "" {
		caption = note + "\n"
	}
	caption += "**" + usedPrompt + "**\n" +
		fmt.Sprintf("seed %d · %dx%d · %.0fs\n", img.Seed, img.Width, img.Height, img.Seconds) +
		"Gallery: " + art.GalleryURL()
	return caption
}

// cutPhotoCmd recognizes the photo commands. A word boundary is
// required, so "!photograph a sunset" stays a chat message. mode is ""
// for plain !photo, "assist" for !photoassist, "check" for !photocheck,
// "now" for !photonow and "assistnow" for !photoassistnow (the top-priority
// variants). The longer commands are tried first so a prefix never
// mis-matches (e.g. !photoassistnow is not cut as !photoassist).
//
// An optional size token may follow the command — "!photo 16x9 a red
// panda" — and is consumed as the requested frame (the service default,
// square 1024x1024, when absent).
func cutPhotoCmd(content string) (mode, size, prompt string, ok bool) {
	for _, c := range []struct {
		cmd  string
		mode string
	}{
		{"!photoassistnow", "assistnow"},
		{"!photoassist", "assist"},
		{"!photocheck", "check"},
		{"!photonow", "now"},
		{"!photo", ""},
	} {
		rest := strings.TrimPrefix(content, c.cmd)
		if rest == content {
			continue
		}
		if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
			continue
		}
		rest = strings.TrimSpace(rest)
		// The first word is a size token ("16x9", "1024x576", "16:9")
		// when it parses as one; otherwise the whole rest is the prompt.
		// A bare size token with no prompt ("!photo 9x16") counts too —
		// handlePhoto asks for a prompt then.
		if i := strings.IndexAny(rest, " \t"); i >= 0 {
			if _, _, isSize := art.ParseSize(rest[:i]); isSize {
				size = rest[:i]
				rest = strings.TrimSpace(rest[i+1:])
			}
		} else if _, _, isSize := art.ParseSize(rest); isSize {
			size = rest
			rest = ""
		}
		return c.mode, size, rest, true
	}
	return "", "", "", false
}

// photoErrorMessage turns a generation failure into something the owner
// can act on.
func photoErrorMessage(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "Photo generation took too long; try again in a bit."
	}
	var busy *art.ErrBusy
	if errors.As(err, &busy) {
		return "Another photo is already generating — try again in a minute."
	}
	var unavail *art.ErrUnavailable
	if errors.As(err, &unavail) {
		return "The art model is not available right now: " + unavail.Detail
	}
	return "Photo generation failed; try again in a moment."
}

// logConversation appends one exchange to the daily conversation log
// (discord-YYYY-MM-DD.md) so the coding agent can read back what was
// talked about. Directory comes from $JARVIS_NOTES_DIR; when the var is
// unset it defaults to <home>/ai-agent-build/notes/conversations, and an
// explicitly empty var disables logging (tests rely on that).
//
// Logging is best-effort: any failure is logged and chat goes on.
var conversationLogMu sync.Mutex

func logConversation(content, answer string) {
	dir, ok := os.LookupEnv("JARVIS_NOTES_DIR")
	if !ok {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, "ai-agent-build", "notes", "conversations")
		}
	}
	if dir == "" {
		return
	}

	now := time.Now()
	ts := now.Format("2006-01-02 15:04:05")
	name := filepath.Join(dir, "discord-"+now.Format("2006-01-02")+".md")

	conversationLogMu.Lock()
	defer conversationLogMu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("jarvis-chat: conversation log: %v", err)
		return
	}
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("jarvis-chat: conversation log: %v", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] Nathan: %s\n\n[%s] Jarvis:\n%s\n\n", ts, content, ts, answer)
}

func typingLoop(ctx context.Context, d sender) {
	ticker := time.NewTicker(typingInterval)
	defer ticker.Stop()
	// Send an immediate typing indicator.
	_ = d.Typing()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = d.Typing()
		}
	}
}

// withReplyContext returns the user turn the brain should see for a chat:
// when the owner Discord-replies to a message, the content of that message
// (and who sent it) is prepended so the brain knows what the reply refers
// to. When there is no reply, the message is returned unchanged.
func withReplyContext(in chat.Incoming) string {
	if strings.TrimSpace(in.ReplyTo) == "" {
		return in.Content
	}
	who := "a message"
	if a := strings.TrimSpace(in.ReplyAuthor); a != "" {
		who = "a message from " + a
	}
	return "You are replying to " + who + ":\n" + in.ReplyTo + "\n\n" + in.Content
}

// buildMessages constructs the Ollama chat messages from the system prompt,
// status line, and conversation history. Uses the default "jarvis" voice.
func buildMessages(userMsg string, history []string) []models.ChatMessage {
	return buildMessagesWithVoice(userMsg, history, voices.Get(""))
}

// buildMessagesWithVoice constructs the Ollama chat messages using the
// given voice's system prompt, the current status line, and conversation
// history.
func buildMessagesWithVoice(userMsg string, history []string, voice *voices.Voice) []models.ChatMessage {
	status := getStatusLine()

	systemPrompt := voice.SystemPrompt
	if status != "" {
		systemPrompt += "\n\nCurrent status:\n" + status
	}

	messages := []models.ChatMessage{
		{Role: "system", Content: systemPrompt},
	}

	// Interleave history: even indices are user, odd are assistant.
	for i := 0; i < len(history); i += 2 {
		messages = append(messages, models.ChatMessage{
			Role:    "user",
			Content: history[i],
		})
		if i+1 < len(history) {
			messages = append(messages, models.ChatMessage{
				Role:    "assistant",
				Content: history[i+1],
			})
		}
	}

	// Add the current message.
	messages = append(messages, models.ChatMessage{
		Role:    "user",
		Content: userMsg,
	})

	return messages
}

// getStatusLine reads the current job and the last 3 lines of agent.log.
func getStatusLine() string {
	confDir := os.Getenv("JARVIS_CONF")
	if confDir == "" {
		home, _ := os.UserHomeDir()
		confDir = filepath.Join(home, ".config", "jarvis")
	}

	var parts []string

	// Current job.
	if b, err := os.ReadFile(confDir + "/current-job"); err == nil {
		job := strings.TrimSpace(string(b))
		if job != "" {
			parts = append(parts, "Job: "+job)
		}
	}

	// Last 3 lines of agent.log.
	logPath := filepath.Join(os.Getenv("HOME"), ".local", "state", "jarvis", "agent.log")
	if lines := tailLines(logPath, 3); len(lines) > 0 {
		parts = append(parts, "Recent log:")
		parts = append(parts, lines...)
	}

	return strings.Join(parts, "\n")
}

// tailLines returns the last n lines of a file.
func tailLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return lines
}

// pruneHistory trims the history to at most maxWords total words.
func pruneHistory(history *[]string, maxWords int) {
	h := *history
	totalWords := 0
	for _, turn := range h {
		totalWords += strings.Count(turn, " ") + 1
	}

	// Remove oldest turns until under the limit.
	for totalWords > maxWords && len(h) >= 2 {
		totalWords -= strings.Count(h[0], " ") + 1
		h = h[1:]
		if len(h) >= 2 {
			totalWords -= strings.Count(h[0], " ") + 1
			h = h[1:]
		}
	}
	*history = h
}

// loadEnvFile fills in unset variables from the env file, if it exists.
// Tries secrets.env first (new format), then discord.env (legacy).
func loadEnvFile() {
	path := os.Getenv("JARVIS_DISCORD_ENV")
	if path == "" {
		home, _ := os.UserHomeDir()
		// Try secrets.env first (new multi-bot format).
		secretsPath := filepath.Join(home, ".config", "jarvis", "secrets.env")
		legacyPath := filepath.Join(home, ".config", "jarvis", "discord.env")
		if _, err := os.Stat(secretsPath); err == nil {
			path = secretsPath
		} else if _, err := os.Stat(legacyPath); err == nil {
			path = legacyPath
		} else {
			return
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || strings.HasPrefix(line, "#") || os.Getenv(k) != "" {
			continue
		}
		os.Setenv(k, strings.Trim(strings.TrimSpace(v), `"'`))
	}
}
