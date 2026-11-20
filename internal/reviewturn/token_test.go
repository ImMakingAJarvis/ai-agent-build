package reviewturn

import (
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// TestToken covers Job 7a: only the token holder may push, and a failed
// check never invokes the push function.
func TestToken(t *testing.T) {
	r := reviewstate.Record{AuthorModel: "local", ReviewerModel: "sonnet", Token: "local"}

	// Pusher holds the token: nil error.
	if err := AllowPush(r, "local"); err != nil {
		t.Errorf("AllowPush(token holder) = %v, want nil", err)
	}

	// Pusher is the other model: error contains "token".
	err := AllowPush(r, "sonnet")
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("AllowPush(other model) = %v, want error containing %q", err, "token")
	}

	// Token empty: error contains "token".
	empty := r
	empty.Token = ""
	err = AllowPush(empty, "local")
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("AllowPush(empty token) = %v, want error containing %q", err, "token")
	}

	// A failed check does not push.
	pushed := false
	err = Push(empty, "sonnet", func() error { pushed = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("Push(failed check) = %v, want error containing %q", err, "token")
	}
	if pushed {
		t.Error("push was invoked although the token check failed")
	}

	// A passed check does push.
	err = Push(r, "local", func() error { pushed = true; return nil })
	if err != nil {
		t.Errorf("Push(passed check) = %v, want nil", err)
	}
	if !pushed {
		t.Error("push was not invoked although the token check passed")
	}
}
