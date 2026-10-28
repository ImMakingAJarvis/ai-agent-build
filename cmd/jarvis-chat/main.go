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
	"log"
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
	busyTimeout  = 20 * time.Second  // send busy notice after this long
	maxTimeout   = 4 * time.Minute   // give up after this long
	photoTimeout = 3 * time.Minute   // one photo generation
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

	ollamaClient := models.NewClient()
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

				// !photo <prompt> generates a photo with the local art
				// model and sends it back as an attachment.
				if prompt, ok := cutPhoto(content); ok {
					handlePhoto(d, photoGen, prompt)
					return
				}

				if kind, rest, ok := cutBuildFix(content); ok {
					class := agentjob.ClassBuild
					if kind == "fix" {
						class = agentjob.ClassFix
					}
					title, body := titleAndBody(rest)
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					if title == "" {
						_ = d.Notify(ctx, "Need a title after !build / !fix.")
						return
					}
					id, err := enqueueFromChat(in, class, title, body)
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

		answerCh := make(chan string, 1)
		errCh2 := make(chan error, 1)
		go func() {
			a, err := hAgent.Chat(ctx, systemPrompt, *history, content)
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

// handlePhoto runs one photo generation for the !photo command and
// sends the result to the owner as an attachment. A 1024x1024 image
// takes tens of seconds; the typing indicator stays up for the whole
// generation.
func handlePhoto(d photoSender, gen *art.Client, prompt string) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = d.Notify(ctx, "Need a prompt after !photo — e.g. !photo a photo of a red panda in fresh snow")
		return
	}

	typingCtx, typingCancel := context.WithCancel(context.Background())
	defer typingCancel()
	go typingLoop(typingCtx, d)

	genCtx, genCancel := context.WithTimeout(context.Background(), photoTimeout)
	img, err := gen.Generate(genCtx, prompt)
	genCancel()
	typingCancel()
	if err != nil {
		log.Printf("jarvis-chat: photo: %v", err)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = d.Notify(ctx, photoErrorMessage(err))
		return
	}

	if len(prompt) > 300 {
		prompt = prompt[:300] + "…"
	}
	caption := fmt.Sprintf("**%s**\nseed %d · %dx%d · %.0fs\nGallery: %s",
		prompt, img.Seed, img.Width, img.Height, img.Seconds, art.GalleryURL())

	// Fresh context: the generation ate most of photoTimeout already.
	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer sendCancel()
	if err := d.SendImage(sendCtx, caption, img.Path); err != nil {
		log.Printf("jarvis-chat: photo send: %v", err)
		_ = d.Notify(sendCtx, "The photo is done ("+img.Path+") but I could not send the file: "+err.Error())
	}
}

// cutPhoto recognizes the "!photo <prompt>" command. A word boundary is
// required, so "!photograph a sunset" stays a chat message.
func cutPhoto(content string) (prompt string, ok bool) {
	rest := strings.TrimPrefix(content, "!photo")
	if rest == content {
		return "", false
	}
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return "", false
	}
	return strings.TrimSpace(rest), true
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
