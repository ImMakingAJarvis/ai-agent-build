// Package chat talks to the owner over Discord DMs.
//
// It is used today by cmd/jarvis-discord (the bootstrap agent's notify/ask
// helpers) and is meant to grow into the Phase 7 two-way bot: enqueueing
// jobs, status replies, and Approve/Reject buttons for cost-gated jobs.
//
// Only the owner is ever listened to: messages and button clicks from any
// other user ID are ignored.
package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// Discord limits message content to 2000 characters.
const maxMessageLen = 1900

type Config struct {
	Token   string // bot token
	OwnerID string // the only Discord user ID the bot will talk to
	// SignalFile, if non-empty, is written whenever the owner sends a
	// message and there is no pending Ask waiting for a reply. The agent-
	// run loop monitors this file and pauses the current job when it
	// appears, so the agent can answer the user immediately.
	SignalFile string
}

type Discord struct {
	cfg  Config
	s    *discordgo.Session
	dmID string

	openOnce sync.Once
	openErr  error

	mu      sync.Mutex
	pending []*waiter // oldest first

	// messageHook, if non-nil, is called for every owner DM that is not
	// consumed by a pending Ask waiter. The daemon can use it to process
	// free-form messages.
	messageHook func(string)
}

// waiter is one open question.
type waiter struct {
	token string // embedded in button custom IDs
	msgID string // the message carrying the question (and its buttons)
	reply chan string
}

func NewDiscord(cfg Config) (*Discord, error) {
	if cfg.Token == "" || cfg.OwnerID == "" {
		return nil, errors.New("discord: token and owner ID are required")
	}
	s, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsDirectMessages

	ch, err := s.UserChannelCreate(cfg.OwnerID)
	if err != nil {
		return nil, fmt.Errorf("discord: open DM with owner (the bot must share a server with you): %w", err)
	}

	d := &Discord{cfg: cfg, s: s, dmID: ch.ID}
	s.AddHandler(d.onMessage)
	s.AddHandler(d.onInteraction)
	return d, nil
}

// Close disconnects from the gateway if Ask opened it.
func (d *Discord) Close() error {
	return d.s.Close()
}

// Listen opens the Discord gateway and blocks until context cancellation.
// It is used by the watch command to keep a persistent connection that
// fires onMessage for every incoming DM.
func (d *Discord) Listen() error {
	return d.listen()
}

// Notify sends text to the owner. It uses REST only, so it doesn't need a
// gateway connection.
func (d *Discord) Notify(ctx context.Context, text string) error {
	for _, chunk := range split(text) {
		if _, err := d.s.ChannelMessageSend(d.dmID, chunk, discordgo.WithContext(ctx)); err != nil {
			return err
		}
	}
	return nil
}

// Typing sends a typing indicator to the owner's DM channel.
// Discord ignores rapid repeats, so calling this every few seconds is
// safe and keeps the indicator alive for long operations.
func (d *Discord) Typing() error {
	return d.s.ChannelTyping(d.dmID)
}

// OnMessage registers a callback that fires for every owner DM that is
// not consumed by a pending Ask waiter. The callback receives the
// message content string.
func (d *Discord) OnMessage(fn func(string)) {
	d.messageHook = fn
}

// Ask sends question to the owner and blocks until they answer, either by
// clicking one of choices (if any) or by typing a reply. With several
// questions open, a typed reply goes to the one it Discord-replies to, or
// else to the oldest open question.
func (d *Discord) Ask(ctx context.Context, question string, choices []string) (string, error) {
	if err := d.listen(); err != nil {
		return "", err
	}

	w := &waiter{token: newToken(), reply: make(chan string, 1)}
	chunks := split(question)
	for _, chunk := range chunks[:len(chunks)-1] {
		if _, err := d.s.ChannelMessageSend(d.dmID, chunk, discordgo.WithContext(ctx)); err != nil {
			return "", err
		}
	}
	msg, err := d.s.ChannelMessageSendComplex(d.dmID, &discordgo.MessageSend{
		Content:    chunks[len(chunks)-1],
		Components: buttons(w.token, choices),
	}, discordgo.WithContext(ctx))
	if err != nil {
		return "", err
	}
	w.msgID = msg.ID

	d.mu.Lock()
	d.pending = append(d.pending, w)
	d.mu.Unlock()
	defer d.remove(w)

	select {
	case answer := <-w.reply:
		return answer, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (d *Discord) listen() error {
	d.openOnce.Do(func() { d.openErr = d.s.Open() })
	return d.openErr
}

func (d *Discord) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.ID != d.cfg.OwnerID || m.ChannelID != d.dmID {
		return
	}

	d.mu.Lock()
	var target *waiter
	if ref := m.MessageReference; ref != nil {
		for _, w := range d.pending {
			if w.msgID == ref.MessageID {
				target = w
				break
			}
		}
	}
	if target == nil && len(d.pending) > 0 {
		target = d.pending[0]
	}
	d.mu.Unlock()

	if target != nil && deliver(target, m.Content) {
		s.MessageReactionAdd(m.ChannelID, m.ID, "✅")
		return
	}
	// No pending waiter: if signal file is configured, write the user's
	// message there so the agent-run loop can pause the current job.
	if d.cfg.SignalFile != "" && target == nil {
		os.WriteFile(d.cfg.SignalFile, []byte(m.Content), 0o644)
		}
		if d.messageHook != nil && target == nil {
			d.messageHook(m.Content)
		}
	}

func (d *Discord) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionMessageComponent || i.User == nil || i.User.ID != d.cfg.OwnerID {
		return
	}
	token, choice, ok := parseCustomID(i.MessageComponentData().CustomID)
	if !ok {
		return
	}

	d.mu.Lock()
	var target *waiter
	for _, w := range d.pending {
		if w.token == token {
			target = w
			break
		}
	}
	d.mu.Unlock()

	// Another process may own this question; leave the click for it.
	if target == nil || !deliver(target, choice) {
		return
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    i.Message.Content + "\n→ **" + choice + "**",
			Components: []discordgo.MessageComponent{},
		},
	})
}

func (d *Discord) remove(w *waiter) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for n, p := range d.pending {
		if p == w {
			d.pending = append(d.pending[:n], d.pending[n+1:]...)
			return
		}
	}
}

// deliver hands answer to w unless it already has one.
func deliver(w *waiter, answer string) bool {
	select {
	case w.reply <- answer:
		return true
	default:
		return false
	}
}

func buttons(token string, choices []string) []discordgo.MessageComponent {
	if len(choices) == 0 {
		return nil
	}
	// Discord allows 5 buttons per row and 5 rows per message.
	var rows []discordgo.MessageComponent
	var row discordgo.ActionsRow
	for n, c := range choices {
		if n == 25 {
			break
		}
		row.Components = append(row.Components, discordgo.Button{
			Label:    c,
			Style:    discordgo.SecondaryButton,
			CustomID: "ask:" + token + ":" + c,
		})
		if len(row.Components) == 5 {
			rows = append(rows, row)
			row = discordgo.ActionsRow{}
		}
	}
	if len(row.Components) > 0 {
		rows = append(rows, row)
	}
	return rows
}

func parseCustomID(id string) (token, choice string, ok bool) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 || parts[0] != "ask" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func newToken() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// split breaks text into chunks Discord will accept, preferring line breaks.
func split(text string) []string {
	if text == "" {
		return []string{"(empty)"}
	}
	var chunks []string
	for len(text) > maxMessageLen {
		cut := strings.LastIndex(text[:maxMessageLen], "\n")
		if cut <= 0 {
			cut = maxMessageLen
		}
		chunks = append(chunks, text[:cut])
		text = strings.TrimPrefix(text[cut:], "\n")
	}
	return append(chunks, text)
}
