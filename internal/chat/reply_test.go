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
	if inc.ReplyTo != "" || inc.ReplyAuthor != "" {
		t.Errorf("nil message should be a no-op, got %+v", inc)
	}

	// Whitespace-only content (attachment/embed only) is a no-op.
	inc = Incoming{}
	applyReply(&discordgo.Message{Content: "   \n"}, &inc)
	if inc.ReplyTo != "" || inc.ReplyAuthor != "" {
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

	// No author: content is still recorded, author stays empty.
	inc = Incoming{}
	applyReply(&discordgo.Message{Content: "ship it?"}, &inc)
	if inc.ReplyTo != "ship it?" {
		t.Errorf("ReplyTo = %q, want %q", inc.ReplyTo, "ship it?")
	}
	if inc.ReplyAuthor != "" {
		t.Errorf("ReplyAuthor = %q, want empty", inc.ReplyAuthor)
	}
}
