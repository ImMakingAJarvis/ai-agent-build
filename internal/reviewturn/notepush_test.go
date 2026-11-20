package reviewturn

import (
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// TestPush covers Job 7b: a push updates the stamps, and the turn ends with
// the token on the other model and TurnPushed false.
func TestPush(t *testing.T) {
	r := reviewstate.Record{
		PR: 42, Head: "aaa",
		AuthorModel: "local", ReviewerModel: "sonnet", Token: "local",
		AuthorStamp: "aaa", ReviewerStamp: "aaa",
	}

	// Author holds the token and pushes a new commit.
	got, err := NotePush(r, "local", "bbb")
	if err != nil {
		t.Fatalf("NotePush = %v", err)
	}
	if got.Token != "sonnet" {
		t.Errorf("token = %q, want the reviewer %q", got.Token, "sonnet")
	}
	if got.AuthorStamp != "bbb" {
		t.Errorf("author stamp = %q, want the new SHA %q", got.AuthorStamp, "bbb")
	}
	if got.ReviewerStamp != "" {
		t.Errorf("reviewer stamp = %q, want empty", got.ReviewerStamp)
	}
	if got.TurnPushed {
		t.Error("TurnPushed = true after NotePush, want false (the turn is over)")
	}
	if got.Head != "bbb" {
		t.Errorf("head = %q, want %q", got.Head, "bbb")
	}

	// A pusher without the token neither pushes nor ends the turn.
	_, err = NotePush(r, "sonnet", "ccc")
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("NotePush(other model) = %v, want error containing %q", err, "token")
	}
}
