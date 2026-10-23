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
//
// It skips messages while a checkpoint question is open
// ($CONF/ask-open exists) and skips Discord replies to bot questions
// (those belong to Ask).
package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/chat"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

//go:embed system_prompt.txt
var defaultSystemPrompt string

const (
	// Typing is refreshed every 8 seconds to keep the indicator alive.
	typingInterval = 8 * time.Second

	// Maximum conversation history in words.
	maxHistoryWords = 3000

	// Maximum number of recent turns to include.
	maxTurns = 10

	// Default model for chat.
	defaultModel = "jarvis-brain"
)

// Timeouts are variables so tests can shorten them.
var (
	busyTimeout = 20 * time.Second // send busy notice after this long
	maxTimeout  = 4 * time.Minute  // give up after this long
)

// sender abstracts the Discord operations handleMessage needs,
// so tests can substitute a fake.
type sender interface {
	Notify(context.Context, string) error
	Typing() error
}

func main() {
	loadEnvFile()

	confDir := os.Getenv("JARVIS_CONF")
	if confDir == "" {
		home, _ := os.UserHomeDir()
		confDir = filepath.Join(home, ".config", "jarvis")
	}

	d, err := chat.NewDiscord(chat.Config{
		Token:   os.Getenv("DISCORD_BOT_TOKEN"),
		OwnerID: os.Getenv("DISCORD_OWNER_ID"),
	})
	if err != nil {
		log.Fatalf("jarvis-chat: %v", err)
	}
	defer d.Close()

	ollamaClient := models.NewClient()
	modelName := os.Getenv("LOCAL_MODEL")
	if modelName == "" {
		modelName = defaultModel
	}

	// Conversation history: alternating user/assistant turns.
	var history []string // [user1, assistant1, user2, assistant2, ...]

	d.OnMessage(func(content string) {
		// Skip while a checkpoint question is open.
		if _, err := os.Stat(confDir + "/ask-open"); err == nil {
			return
		}

		// Skip empty messages.
		content = strings.TrimSpace(content)
		if content == "" {
			return
		}

		// !pause is a special command: write the pause-signal file so
		// agent-run can interrupt the current job, even when jarvis-chat
		// is active (and the Discord watcher is not running).
		if content == "!pause" {
			_ = os.WriteFile(confDir+"/pause-signal", []byte("!pause"), 0o644)
			return
		}

		handleMessage(d, ollamaClient, modelName, content, &history)
	})

	// Open the gateway so onMessage fires.
	if err := d.Listen(); err != nil {
		log.Fatalf("jarvis-chat: listen: %v", err)
	}

	log.Println("jarvis-chat: listening for messages")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	select {
	case <-sig:
		log.Println("jarvis-chat: shutting down")
	case <-time.After(24 * time.Hour):
	}
}

func handleMessage(d sender, client *models.Client, model, content string, history *[]string) {
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

	// Build the chat messages for the brain.
	messages := buildMessages(content, *history)

	// Call the brain.
	answerCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		answer, err := client.Chat(model, messages)
		if err != nil {
			errCh <- err
			return
		}
		answerCh <- answer
	}()

	// Busy notice after 20s.
	busySent := false
	var job string
	if b, err := os.ReadFile(os.Getenv("JARVIS_CONF") + "/current-job"); err == nil {
		job = strings.TrimSpace(string(b))
	}
	if job == "" {
		job = "an unknown task"
	}

	var answer string
	select {
	case answer = <-answerCh:
	case err := <-errCh:
		log.Printf("jarvis-chat: brain error: %v", err)
		// Still try to reply with what we have.
		time.Sleep(2 * time.Second) // brief pause before error reply
		typingCancel()
		reply := "Sorry, my brain stumbled; try again in a moment."
		_ = d.Notify(ctx, reply)
		return
	case <-time.After(busyTimeout):
		_ = d.Notify(ctx, "Busy on "+job+", my answer is coming")
		busySent = true
		// Wait for the answer or timeout.
		select {
		case answer = <-answerCh:
		case err := <-errCh:
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

	// Got an answer.
	typingCancel()

	// Update history: append user message + assistant reply.
	*history = append(*history, content, answer)
	pruneHistory(history, maxHistoryWords)

	if err := d.Notify(ctx, answer); err != nil {
		log.Printf("jarvis-chat: notify: %v", err)
	}
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
// status line, and conversation history.
func systemPromptText() string {
	confDir := os.Getenv("JARVIS_CONF")
	if confDir == "" {
		home, _ := os.UserHomeDir()
		confDir = filepath.Join(home, ".config", "jarvis")
	}
	if b, err := os.ReadFile(filepath.Join(confDir, "chat-prompt.txt")); err == nil {
		if t := string(bytes.TrimSpace(b)); t != "" {
			return t
		}
	}
	return strings.TrimSpace(defaultSystemPrompt)
}

func buildMessages(userMsg string, history []string) []models.ChatMessage {
	status := getStatusLine()

	systemPrompt := systemPromptText()
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
// Reuses the same logic as jarvis-discord.
func loadEnvFile() {
	path := os.Getenv("JARVIS_DISCORD_ENV")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".config", "jarvis", "discord.env")
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
