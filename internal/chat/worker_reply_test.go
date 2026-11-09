package chat

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// onMessage is the gateway handler the daemon registers, so calling it
// directly with a crafted MessageCreate covers the real path: owner check,
// waiter lookup, reply attachment, worker-reply routing, signal file and
// hook dispatch. Only the REST fetch (attachReply) and the reaction go
// through the session, and both are answered by fakeTransport below.

const (
	testOwnerID   = "1"
	testChannelID = "11"
	testBotID     = "9"
)

// fakeTransport records every REST call the session makes and answers the
// channel-message fetch with a canned Discord message JSON, so onMessage
// runs without a gateway or a live API.
type fakeTransport struct {
	mu    sync.Mutex
	calls []string // "METHOD /path", in order
	get   []byte   // body for the GET message fetch
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req.Method+" "+req.URL.Path)
	f.mu.Unlock()
	var body []byte
	if req.Method == http.MethodGet {
		body = f.get
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func (f *fakeTransport) hadReaction() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, "PUT ") && strings.Contains(c, "/reactions/") {
			return true
		}
	}
	return false
}

func newTestDiscord(t *testing.T, dataDir string, ft *fakeTransport) *Discord {
	t.Helper()
	s, err := discordgo.New("Bot abc.def.ghi")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	s.Client.Transport = ft
	return &Discord{
		cfg:  Config{OwnerID: testOwnerID, SignalFile: filepath.Join(dataDir, "signal")},
		s:    s,
		dmID: testChannelID,
	}
}

// inboxTexts returns the pending inbox message bodies under dataDir.
func inboxTexts(t *testing.T, dataDir string) []string {
	t.Helper()
	dir := filepath.Join(dataDir, "inbox")
	ents, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read inbox: %v", err)
	}
	var texts []string
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		texts = append(texts, string(b))
	}
	return texts
}

// A reply to a bot message is a reply to the worker: it must land in the
// agent inbox (with the worker message quoted), get a 👍, and stay away
// from the signal file and every message hook, so no chat handler can
// answer it or schedule a job from it.
func TestOnMessageRoutesWorkerReplyToInbox(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dataDir)

	ft := &fakeTransport{get: []byte(
		`{"id":"99","channel_id":"11","content":"PR #195 merged, conflict sweep done",` +
			`"author":{"id":"9","username":"jarvis","bot":true}}`)}
	d := newTestDiscord(t, dataDir, ft)
	hooked := false
	d.OnMessage(func(inc Incoming) {
		hooked = true
		if inc.ReplyToBot {
			t.Errorf("message hook must never see a worker reply, got %+v", inc)
		}
	})

	d.onMessage(d.s, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID:               "21",
		ChannelID:        testChannelID,
		Content:          "great, now do the notes sweep",
		Author:           &discordgo.User{ID: testOwnerID, Username: "nathan"},
		MessageReference: &discordgo.MessageReference{MessageID: "99"},
	}})

	// The worker reads it: exactly one inbox message, with the worker
	// message quoted above the owner's words.
	texts := inboxTexts(t, dataDir)
	if len(texts) != 1 {
		t.Fatalf("inbox has %d messages, want 1: %q", len(texts), texts)
	}
	want := "You are replying to a message from the worker:\n" +
		"PR #195 merged, conflict sweep done\n\n" +
		"great, now do the notes sweep"
	if texts[0] != want {
		t.Errorf("inbox text = %q, want %q", texts[0], want)
	}

	// The worker acknowledges: a reaction on the owner's message.
	if !ft.hadReaction() {
		t.Errorf("no reaction recorded, calls: %v", ft.calls)
	}

	// The chat ignores it: no hook, no signal file.
	if hooked {
		t.Error("message hook fired for a worker reply")
	}
	if _, err := os.Stat(d.cfg.SignalFile); !os.IsNotExist(err) {
		t.Error("signal file written for a worker reply")
	}
}

// A reply to a bot's pure-attachment message (no textual content) is still
// a reply to the worker: it routes to the inbox, with nothing to quote.
func TestOnMessageRoutesWorkerReplyWithoutContext(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dataDir)

	ft := &fakeTransport{get: []byte(
		`{"id":"99","channel_id":"11","content":"",` +
			`"author":{"id":"9","username":"jarvis","bot":true}}`)}
	d := newTestDiscord(t, dataDir, ft)
	hooked := false
	d.OnMessage(func(Incoming) { hooked = true })

	d.onMessage(d.s, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID:               "21",
		ChannelID:        testChannelID,
		Content:          "the photo is wrong, regenerate",
		Author:           &discordgo.User{ID: testOwnerID, Username: "nathan"},
		MessageReference: &discordgo.MessageReference{MessageID: "99"},
	}})

	texts := inboxTexts(t, dataDir)
	if len(texts) != 1 || texts[0] != "the photo is wrong, regenerate" {
		t.Fatalf("inbox = %q, want one bare message", texts)
	}
	if !ft.hadReaction() {
		t.Errorf("no reaction recorded, calls: %v", ft.calls)
	}
	if hooked {
		t.Error("message hook fired for a worker reply")
	}
}

// A reply to a human message keeps the old behaviour: the hook and the
// signal file both see it, with the quoted context, and nothing lands in
// the inbox.
func TestOnMessageUserReplyUnchanged(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dataDir)

	ft := &fakeTransport{get: []byte(
		`{"id":"99","channel_id":"11","content":"what did we do today",` +
			`"author":{"id":"1","username":"nathan","bot":false}}`)}
	d := newTestDiscord(t, dataDir, ft)
	var got Incoming
	d.OnMessage(func(inc Incoming) { got = inc })

	d.onMessage(d.s, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID:               "21",
		ChannelID:        testChannelID,
		Content:          "the notes sweep",
		Author:           &discordgo.User{ID: testOwnerID, Username: "nathan"},
		MessageReference: &discordgo.MessageReference{MessageID: "99"},
	}})

	if got.Content != "the notes sweep" || got.ReplyTo != "what did we do today" {
		t.Errorf("hook got %+v", got)
	}
	if got.ReplyToBot {
		t.Error("a reply to a human must not be flagged ReplyToBot")
	}
	if _, err := os.Stat(d.cfg.SignalFile); err != nil {
		t.Errorf("signal file not written for a normal owner message: %v", err)
	}
	if texts := inboxTexts(t, dataDir); len(texts) != 0 {
		t.Errorf("inbox has %d messages, want 0: %q", len(texts), texts)
	}
}

// A plain message (no Discord reply) makes no REST call at all and still
// reaches the hook and the signal file.
func TestOnMessagePlainMessageMakesNoRESTCall(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dataDir)

	ft := &fakeTransport{}
	d := newTestDiscord(t, dataDir, ft)
	var got Incoming
	d.OnMessage(func(inc Incoming) { got = inc })

	d.onMessage(d.s, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID:        "21",
		ChannelID: testChannelID,
		Content:   "hello",
		Author:    &discordgo.User{ID: testOwnerID, Username: "nathan"},
	}})

	if got.Content != "hello" {
		t.Errorf("hook got %+v", got)
	}
	if ft.calls != nil {
		t.Errorf("REST calls for a non-reply message: %v", ft.calls)
	}
	if _, err := os.Stat(d.cfg.SignalFile); err != nil {
		t.Errorf("signal file not written: %v", err)
	}
}
