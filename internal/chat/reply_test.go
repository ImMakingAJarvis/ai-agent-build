package chat

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

// applyReply is the pure half of the reply-context feature: it decides what
// from a replied-to message becomes the Incoming.ReplyTo / ReplyAuthor. The
// REST fetch (attachReply) is a thin wrapper around it and needs a live
// session, so the decision logic is tested here.
func TestApplyReply(t *testing.T) {
	// A nil message is a no-op (e.g. the fetch returned nothing).
	inc := Incoming{}
	applyReply(nil, &inc)
	if inc.ReplyTo != "" || inc.ReplyAuthor != "" || inc.ReplyToBot {
		t.Errorf("nil message should be a no-op, got %+v", inc)
	}

	// Whitespace-only content (attachment/embed only) is a no-op.
	inc = Incoming{}
	applyReply(&discordgo.Message{Content: "   \n"}, &inc)
	if inc.ReplyTo != "" || inc.ReplyAuthor != "" || inc.ReplyToBot {
		t.Errorf("empty content should be a no-op, got %+v", inc)
	}

	// Normal case: content and author are recorded.
	inc = Incoming{}
	applyReply(&discordgo.Message{
		Content: "how did we fix the redis flag store?",
		Author:  &discordgo.User{Username: "nathan"},
	}, &inc)
	if inc.ReplyTo != "how did we fix the redis flag store?" {
		t.Errorf("ReplyTo = %q", inc.ReplyTo)
	}
	if inc.ReplyAuthor != "nathan" {
		t.Errorf("ReplyAuthor = %q", inc.ReplyAuthor)
	}
	if inc.ReplyToBot {
		t.Error("a human author should not set ReplyToBot")
	}

	// No author: content is still recorded, author stays empty.
	inc = Incoming{}
	applyReply(&discordgo.Message{Content: "ship it?"}, &inc)
	if inc.ReplyTo != "ship it?" {
		t.Errorf("ReplyTo = %q, want %q", inc.ReplyTo, "ship it?")
	}
	if inc.ReplyAuthor != "" {
		t.Errorf("ReplyAuthor = %q, want empty", inc.ReplyAuthor)
	}
	if inc.ReplyToBot {
		t.Error("a missing author should not set ReplyToBot")
	}

	// Bot author: ReplyToBot is set so onMessage can route the reply to the
	// worker, and the content is still recorded as context.
	inc = Incoming{}
	applyReply(&discordgo.Message{
		Content: "job chat-123 is done (PR #195)",
		Author:  &discordgo.User{Username: "jarvis", Bot: true},
	}, &inc)
	if !inc.ReplyToBot {
		t.Error("a bot author should set ReplyToBot")
	}
	if inc.ReplyTo != "job chat-123 is done (PR #195)" {
		t.Errorf("ReplyTo = %q", inc.ReplyTo)
	}
	if inc.ReplyAuthor != "jarvis" {
		t.Errorf("ReplyAuthor = %q", inc.ReplyAuthor)
	}

	// Bot author with no textual content (a pure attachment): still a
	// reply to the worker, but there is no context to quote.
	inc = Incoming{}
	applyReply(&discordgo.Message{
		Content: "",
		Author:  &discordgo.User{Username: "jarvis", Bot: true},
	}, &inc)
	if !inc.ReplyToBot {
		t.Error("a bot author should set ReplyToBot even with empty content")
	}
	if inc.ReplyTo != "" || inc.ReplyAuthor != "" {
		t.Errorf("empty content should leave the context empty, got %+v", inc)
	}
}

// workerReplyText builds the inbox payload for a reply to a worker message:
// the worker message quoted above the owner's words, so the round that reads
// it sees the whole exchange.
func TestWorkerReplyText(t *testing.T) {
	got := workerReplyText(Incoming{
		ReplyTo: "PR #195 merged, the conflict sweep is done",
		Content: "great — now do the notes sweep",
	})
	want := "You are replying to a message from the worker:\n" +
		"PR #195 merged, the conflict sweep is done\n\n" +
		"great — now do the notes sweep"
	if got != want {
		t.Errorf("workerReplyText = %q, want %q", got, want)
	}

	// No quoted message (the fetch failed, or it had no text): just the words.
	got = workerReplyText(Incoming{Content: "ship it"})
	if got != "ship it" {
		t.Errorf("workerReplyText = %q, want %q", got, "ship it")
	}

	// A whitespace-only quoted message is treated as no quoted message.
	got = workerReplyText(Incoming{ReplyTo: "  \n", Content: "ship it"})
	if got != "ship it" {
		t.Errorf("workerReplyText = %q, want %q", got, "ship it")
	}
}
