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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/inbox"
	"github.com/bwmarrin/discordgo"
)

// Discord limits message content to 2000 characters.
const maxMessageLen = 1900

// replyFetchTimeout bounds how long we wait to retrieve the message the
// owner is replying to. It runs in the per-message handler goroutine, so a
// slow or dead fetch never holds up the gateway; it just fails open.
const replyFetchTimeout = 5 * time.Second

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
	messageHook func(Incoming)
}

// Incoming is an owner DM that was not consumed by Ask.
type Incoming struct {
	ChannelID string
	MessageID string
	Content   string
	// ReplyTo is the content of the message this one Discord-replies to,
	// when the owner used a reply. Empty when the message is not a reply
	// (or the reference could not be resolved).
	ReplyTo string
	// ReplyAuthor is the username of the author of the replied-to message.
	// Empty when unknown.
	ReplyAuthor string
	// ReplyToBot is true when the owner Discord-replied to a message sent
	// by a bot — that is, to a message the worker sent. Such a reply is
	// for the worker (the agent inbox), not for the chat: onMessage routes
	// it there before any hook or signal file sees it, so a chat handler
	// can never classify it or schedule a job from it.
	ReplyToBot bool
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

// SendImage sends text plus one local image file as an attachment in
// the owner DM. A 1024x1024 PNG from the art model is a few MB, well
// under Discord's 25 MB upload limit.
func (d *Discord) SendImage(ctx context.Context, text, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = d.s.ChannelMessageSendComplex(d.dmID, &discordgo.MessageSend{
		Content: text,
		Files:   []*discordgo.File{{Name: filepath.Base(path), ContentType: "image/png", Reader: f}},
	}, discordgo.WithContext(ctx))
	return err
}

// Typing sends a typing indicator to the owner's DM channel.
// Discord ignores rapid repeats, so calling this every few seconds is
// safe and keeps the indicator alive for long operations.
func (d *Discord) Typing() error {
	return d.s.ChannelTyping(d.dmID)
}

// OnMessage registers a callback that fires for every owner DM that is
// not consumed by a pending Ask waiter.
func (d *Discord) OnMessage(fn func(Incoming)) {
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

	inc := Incoming{ChannelID: m.ChannelID, MessageID: m.ID, Content: m.Content}
	d.attachReply(s, m, &inc)

	// A Discord reply to a message a bot sent is a reply to the worker:
	// it is for the agent, not the chat. Route it to the agent inbox and
	// thumb it up, and keep it away from the signal file and every
	// message hook, so no chat handler can answer it or schedule a job
	// from it.
	if inc.ReplyToBot {
		d.handleWorkerReply(s, m, &inc)
		return
	}

	// No pending waiter: if signal file is configured, write the user's
	// message there so the agent-run loop can pause the current job.
	if d.cfg.SignalFile != "" {
		os.WriteFile(d.cfg.SignalFile, []byte(m.Content), 0o644)
	}
	if d.messageHook != nil {
		d.messageHook(inc)
	}
}

// handleWorkerReply routes an owner message that Discord-replies to a bot
// (worker) message to the agent inbox and acknowledges it with a 👍
// reaction, so the owner knows the worker got it. The worker — not the
// chat — reads it: the running round picks it up via the owner-message
// hook (scripts/owner-message-hook.sh), an idle agent at the next round's
// start. The replied-to message's content is included so a fresh round
// knows what the reply refers to. The reaction is added only when the
// inbox write succeeded: a 👍 means the worker will read it.
func (d *Discord) handleWorkerReply(s *discordgo.Session, m *discordgo.MessageCreate, inc *Incoming) {
	if _, err := inbox.Send(workerReplyText(*inc)); err != nil {
		// The message is lost; the owner can resend. Nothing to react to.
		return
	}
	s.MessageReactionAdd(m.ChannelID, m.ID, "👍")
}

// workerReplyText is the inbox text for a reply to a worker message: the
// owner's words, with the worker message being replied to quoted above so
// the round that reads it has the full exchange.
func workerReplyText(inc Incoming) string {
	if strings.TrimSpace(inc.ReplyTo) == "" {
		return inc.Content
	}
	return "You are replying to a message from the worker:\n" +
		inc.ReplyTo + "\n\n" + inc.Content
}

// attachReply fetches the message m Discord-replies to (when m carries a
// reference) and records its content and author on inc, so the chat handler
// can take the replied-to message into the context. It fails open: on any
// error, or when the reference has no textual content, inc is left
// unchanged.
func (d *Discord) attachReply(s *discordgo.Session, m *discordgo.MessageCreate, inc *Incoming) {
	ref := m.MessageReference
	if ref == nil || ref.MessageID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), replyFetchTimeout)
	defer cancel()
	refMsg, err := s.ChannelMessage(m.ChannelID, ref.MessageID, discordgo.WithContext(ctx))
	if err != nil {
		return
	}
	applyReply(refMsg, inc)
}

// applyReply records the content, author and bot-ness of the replied-to
// message on inc. ReplyToBot is set whenever the author is a bot, even
// when the message has no textual content (a reply to a bot's pure
// attachment still counts as a reply to the worker). The ReplyTo /
// ReplyAuthor fields are left empty when the message has no textual
// content (for example a pure attachment or embed), so the handler only
// ever gets real context.
func applyReply(refMsg *discordgo.Message, inc *Incoming) {
	if refMsg == nil {
		return
	}
	if refMsg.Author != nil && refMsg.Author.Bot {
		inc.ReplyToBot = true
	}
	if strings.TrimSpace(refMsg.Content) == "" {
		return
	}
	inc.ReplyTo = refMsg.Content
	if refMsg.Author != nil {
		inc.ReplyAuthor = refMsg.Author.Username
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
