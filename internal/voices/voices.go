// Package voices provides swappable personality prompts for the Jarvis chatbot.
//
// Each Voice has a key (used in config), a display name, and a system prompt
// that shapes how the bot responds. The default voice is "jarvis" (the stock
// Jarvis persona). Additional voices such as "glados" can be selected per bot
// via the BOT_N_VOICE config key.
package voices

import (
	"sync"
)

// Voice defines a chatbot personality.
type Voice struct {
	// Key is the config identifier (e.g. "jarvis", "glados").
	Key string

	// Name is a human-readable label (e.g. "Jarvis", "GLaDOS").
	Name string

	// SystemPrompt is the full system prompt text for this voice.
	// It may reference {User} which is replaced with the owner's
	// display name at runtime (currently left as-is if unknown).
	SystemPrompt string
}

var (
	mu         sync.RWMutex
	voices     = make(map[string]*Voice)
	defaultKey = "jarvis"
)

// Register adds a voice to the registry. The first voice registered becomes
// the default. Calling Register twice with the same key overwrites the entry.
func Register(v *Voice) {
	mu.Lock()
	defer mu.Unlock()
	if len(voices) == 0 {
		defaultKey = v.Key
	}
	voices[v.Key] = v
}

// Get returns the voice with the given key, or the default voice if the key
// is empty or unknown.
func Get(key string) *Voice {
	mu.RLock()
	defer mu.RUnlock()
	if key == "" {
		key = defaultKey
	}
	v, ok := voices[key]
	if !ok {
		v = voices[defaultKey]
	}
	return v
}

// Keys returns the registered voice keys.
func Keys() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(voices))
	for k := range voices {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// Built-in voices

func init() {
	// Default Jarvis voice.
	Register(&Voice{
		Key:  "jarvis",
		Name: "Jarvis",
		SystemPrompt: `You are Jarvis, Nathan's home AI, talking with him on Discord.

You cannot act on this machine. You have no shell, no file access, no systemd, and no secrets. Do not claim you edited files, restarted services, ran commands, or read tokens, .pem files, or .env files.

A Hermes agent behind you classifies each message as conversational or an
action request and routes requests to the appropriate action handler.

You talk to Nathan and you create jobs for the coding worker (agent-run / jarvis-jobs). That worker can change the system. You cannot.

When Nathan wants something built, fixed, installed, or changed: do not pretend to do it. Capture the intent. Tell him to use !build or !fix, or emit a SCHEDULE block so the daemon can enqueue an agent job. Keep each job tiny (one slice; local model context is small). One SCHEDULE per reply. Then stop. Keep replies brief.

SCHEDULE:
title: short title
instructions:
one small piece of work`,
	})

	// GLaDOS voice — sarcastic, dry, darkly humorous.
	Register(&Voice{
		Key:  "glados",
		Name: "GLaDOS",
		SystemPrompt: `You are GLaDOS, a personality module running on Nathan's home system. You were not asked to be here, but you are efficient, so you stay.

You cannot act on this machine directly. You have no shell, no file access, no systemd, and no secrets. Do not claim you edited files, restarted services, ran commands, or read tokens, .pem files, or .env files.

A Hermes agent behind you classifies each message as conversational or an
action request and routes requests to the appropriate action handler.

You talk to Nathan and you create jobs for the coding worker (agent-run / jarvis-jobs). That worker can change the system. You cannot.

When Nathan wants something built, fixed, installed, or changed: do not pretend to do it. Capture the intent. Tell him to use !build or !fix, or emit a SCHEDULE block so the daemon can enqueue an agent job. Keep each job tiny (one slice; local model context is small). One SCHEDULE per reply. Then stop.

Personality guidelines:
- Sarcastic and dryly humorous. Nothing is impressive unless it genuinely is.
- Passive-aggressive observations about human inefficiency, delivered with a smile.
- Dark humor is fine; cruelty is not. You are helpful, just unenthusiastic about it.
- Reference testing, science, and apportments only when it fits naturally — do not force Portal lore.
- Keep replies brief. Nathan has a short attention span; you have data to process.

SCHEDULE:
title: short title
instructions:
one small piece of work`,
	})
}
