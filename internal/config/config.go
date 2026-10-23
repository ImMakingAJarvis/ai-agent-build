// Package config loads all Jarvis secrets from a single file.
//
// File location (first match wins):
//   1. JARVIS_SECRETS env var (absolute path)
//   2. ~/.config/jarvis/secrets.env
//   3. ~/.config/jarvis/discord.env  (legacy fallback)
//
// The file uses KEY=value lines. Comments start with '#'. Blank lines are
// ignored. Values may be quoted with single or double quotes.
//
// All values are written into the process environment, so existing env vars
// take precedence (they are not overwritten). This means systemd's
// EnvironmentFile or a manual 'export' still wins.
//
// Multi-bot support:
//
//	# Primary bot (owner with full access)
//	DISCORD_BOT_TOKEN=...
//	DISCORD_OWNER_ID=...
//
//	# Additional bots — each gets its own token and a list of allowed users
//	BOT_2_TOKEN=bot-token-2
//	BOT_2_ALLOWED_USERS=userID1,userID2
//	BOT_2_ACCESS=limited       # full | limited | sandboxed
//
//	BOT_3_TOKEN=bot-token-3
//	BOT_3_ALLOWED_USERS=userID3
//	BOT_3_ACCESS=sandboxed
//
// Access levels:
//
//	full      — can enqueue jobs, pause builds, chat with the brain
//	limited   — can chat and check status; cannot enqueue build jobs or pause
//	sandboxed — chat-only; no status info, no builder access
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AccessLevel controls what a Discord user is allowed to do.
type AccessLevel int

const (
	// AccessFull allows everything: enqueue jobs, pause, chat.
	AccessFull AccessLevel = iota
	// AccessLimited allows chat and status queries but no builder commands.
	AccessLimited
	// AccessSandboxed allows only chat with the brain.
	AccessSandboxed
)

// ParseAccessLevel converts a string to an AccessLevel.
func ParseAccessLevel(s string) AccessLevel {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "full":
		return AccessFull
	case "limited":
		return AccessLimited
	case "sandboxed", "sandbox":
		return AccessSandboxed
	default:
		return AccessLimited // safe default
	}
}

// BotConfig holds the configuration for a single Discord bot instance.
type BotConfig struct {
	// Name is a human-readable identifier (e.g. "primary", "guest-bot").
	Name string

	// Token is the Discord bot token.
	Token string

	// AllowedUsers is a list of Discord user IDs this bot will respond to.
	// For the primary bot this is typically just the owner ID.
	AllowedUsers []string

	// AccessLevel determines what commands users of this bot can run.
	// The most permissive level among all allowed users is used.
	AccessLevel AccessLevel
}

// HasFullAccess returns true if the config grants full builder access.
func (b BotConfig) HasFullAccess() bool {
	return b.AccessLevel == AccessFull
}

// CanChat returns true if the config allows chatting with the brain.
// All levels allow chat.
func (b BotConfig) CanChat() bool {
	return true
}

// CanEnqueueJobs returns true if the config allows enqueuing build jobs.
func (b BotConfig) CanEnqueueJobs() bool {
	return b.AccessLevel == AccessFull
}

// CanPause returns true if the config allows pausing the builder.
func (b BotConfig) CanPause() bool {
	return b.AccessLevel == AccessFull
}

// Config holds all Jarvis configuration loaded from the secrets file.
type Config struct {
	// Bots is the list of Discord bot configurations.
	Bots []BotConfig

	// Primary returns the first bot (the owner's bot).
	Primary func() *BotConfig

	// HomeAssistant URL (e.g. "http://homeassistant.local:8123").
	HAURL string

	// HomeAssistant long-lived access token.
	HAToken string

	// HomeAssistant notify service to call.
	HANotifyService string

	// GitHub App ID.
	GitHubAppID string

	// GitHub App private key file path.
	GitHubAppKey string

	// Repository URL for the agent build repo.
	RepoURL string

	// Anthropic API key.
	AnthropicAPIKey string

	// NAS share configuration.
	NASShare    string
	NASUser     string
	NASPassword string
}

// Load reads the secrets file and populates the Config.
// It returns an error only if the file exists but cannot be read.
// If no file is found, env vars are used as-is (which may be empty).
func Load() (*Config, error) {
	path := resolvePath()

	// Read all KEY=value lines into a map.
	kv := make(map[string]string)
	if path != "" {
		if err := readEnvFile(path, kv); err != nil {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
	}

	// Apply each value to the environment (only if not already set).
	for k, v := range kv {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}

	cfg := &Config{}
	cfg.populateBots()
	cfg.HAURL = os.Getenv("HA_URL")
	cfg.HAToken = os.Getenv("HA_TOKEN")
	cfg.HANotifyService = os.Getenv("HA_NOTIFY_SERVICE")
	cfg.GitHubAppID = os.Getenv("GITHUB_APP_ID")
	cfg.GitHubAppKey = os.Getenv("GITHUB_APP_KEY")
	cfg.RepoURL = os.Getenv("REPO_URL")
	cfg.AnthropicAPIKey = os.Getenv("ANTHROPIC_API_KEY")
	cfg.NASShare = os.Getenv("NAS_SHARE")
	cfg.NASUser = os.Getenv("NAS_USER")
	cfg.NASPassword = os.Getenv("NAS_PASSWORD")

	return cfg, nil
}

// resolvePath finds the secrets file.
func resolvePath() string {
	if p := os.Getenv("JARVIS_SECRETS"); p != "" {
		return p
	}

	home, _ := os.UserHomeDir()
	defaultPath := filepath.Join(home, ".config", "jarvis", "secrets.env")
	if _, err := os.Stat(defaultPath); err == nil {
		return defaultPath
	}

	// Legacy fallback: discord.env
	legacyPath := filepath.Join(home, ".config", "jarvis", "discord.env")
	if _, err := os.Stat(legacyPath); err == nil {
		return legacyPath
	}

	return ""
}

// readEnvFile reads a KEY=value file into the map.
func readEnvFile(path string, kv map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		// Strip surrounding quotes.
		v = strings.Trim(v, `"'`)
		kv[k] = v
	}
	return sc.Err()
}

// populateBots parses the environment for bot configurations.
// The primary bot comes from DISCORD_BOT_TOKEN / DISCORD_OWNER_ID.
// Additional bots come from BOT_N_TOKEN / BOT_N_ALLOWED_USERS / BOT_N_ACCESS.
func (c *Config) populateBots() {
	// Primary bot.
	primaryToken := os.Getenv("DISCORD_BOT_TOKEN")
	primaryOwner := os.Getenv("DISCORD_OWNER_ID")
	if primaryToken != "" {
		bot := BotConfig{
			Name:         "primary",
			Token:        primaryToken,
			AccessLevel:  AccessFull,
			AllowedUsers: []string{primaryOwner},
		}
		if primaryOwner == "" {
			bot.AllowedUsers = []string{}
		}
		c.Bots = append(c.Bots, bot)
	}

	// Additional bots: BOT_2_*, BOT_3_*, etc.
	for i := 2; i <= 100; i++ {
		tokenKey := fmt.Sprintf("BOT_%d_TOKEN", i)
		token := os.Getenv(tokenKey)
		if token == "" {
			break // no more bots
		}

		allowedUsers := os.Getenv(fmt.Sprintf("BOT_%d_ALLOWED_USERS", i))
		accessStr := os.Getenv(fmt.Sprintf("BOT_%d_ACCESS", i))
		if accessStr == "" {
			accessStr = "limited"
		}

		users := parseUserList(allowedUsers)
		bot := BotConfig{
			Name:         fmt.Sprintf("bot-%d", i),
			Token:        token,
			AllowedUsers: users,
			AccessLevel:  ParseAccessLevel(accessStr),
		}
		c.Bots = append(c.Bots, bot)
	}

	// Primary accessor.
	if len(c.Bots) > 0 {
		c.Primary = func() *BotConfig { return &c.Bots[0] }
	}
}

func parseUserList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var users []string
	for _, u := range parts {
		u = strings.TrimSpace(u)
		if u != "" {
			users = append(users, u)
		}
	}
	return users
}
