package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/chat"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/config"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/hermes"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// hermesRunner wraps the Hermes agent for use as a Discord message handler.
// It provides the same typing/busy/timeout behavior as the basic chat handler
// but routes messages through Hermes for action execution.
type hermesRunner struct {
	agent     *hermes.Agent
	discord   *chat.Discord
	modelName string
	confDir   string
}

// newHermesRunner creates a Hermes runner with the given bot configuration.
func newHermesRunner(botCfg config.BotConfig) *hermesRunner {
	confDir := os.Getenv("JARVIS_CONF")
	if confDir == "" {
		home, _ := os.UserHomeDir()
		confDir = filepath.Join(home, ".config", "jarvis")
	}

	modelName := os.Getenv("LOCAL_MODEL")
	if modelName == "" {
		modelName = defaultModel
	}

	agent := hermes.New(hermes.Config{
		Model:       modelName,
		MaxHistory:  20,
		AccessLevel: botCfg.AccessLevel,
		ChatSlot:    chatSlotID(),
	})

	// Inject the Ollama client.
	ollamaClient := models.NewClient()
	agent.SetOllamaClient(ollamaClient)

	// Register default actions.
	for _, action := range hermes.DefaultActions() {
		agent.Register(action)
	}

	return &hermesRunner{
		agent:     agent,
		modelName: modelName,
		confDir:   confDir,
	}
}

// SetDiscord links the Discord instance to this runner.
func (r *hermesRunner) SetDiscord(d *chat.Discord) {
	r.discord = d
}

// handleMessage processes an incoming message through Hermes.
func (r *hermesRunner) handleMessage(in chat.Incoming) {
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return
	}

	// !pause works on the Hermes runner too.
	if content == "!pause" {
		_ = os.WriteFile(r.confDir+"/pause-signal", []byte("!pause"), 0o644)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), maxTimeout)
	defer cancel()

	// Start typing indicator.
	typingCtx, typingCancel := context.WithCancel(context.Background())
	defer typingCancel()
	go func() {
		ticker := time.NewTicker(typingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-typingCtx.Done():
				return
			case <-ticker.C:
				_ = r.discord.Typing()
			}
		}
	}()

	// Process through Hermes.
	reply, err := r.agent.Process(ctx, content)
	if err != nil {
		log.Printf("hermes runner: process error: %v", err)
		typingCancel()
		_ = r.discord.Notify(ctx, "Sorry, something went wrong processing your request.")
		return
	}

	typingCancel()

	if err := r.discord.Notify(ctx, reply); err != nil {
		log.Printf("hermes runner: notify: %v", err)
	}
}

// hermesSystemPrompt returns the system prompt for the Hermes runner bot.
func hermesSystemPrompt() string {
	return `You are Jarvis Hermes, the worker bot. You are smarter and more capable than the basic chatbot.

You can:
- Search the internet for up-to-date information
- Check system status and manage services
- Enqueue jobs for the coding worker
- Break complex tasks into smaller pieces for the worker
- Have conversations with the user

When the user asks about something you don't know, search the web first.
When the user wants something built, use enqueue_job or delegate to break it into pieces.
Keep replies brief and actionable.`
}
