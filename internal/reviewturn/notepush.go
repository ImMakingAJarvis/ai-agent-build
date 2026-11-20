package reviewturn

import (
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// NotePush records a completed push: it applies the push (the pusher's stamp
// becomes the new SHA, the other stamp is cleared, the head moves) and then
// ends the turn. After a successful push the token is the other model and
// TurnPushed is false, because the turn is over.
func NotePush(r reviewstate.Record, pusher, sha string) (reviewstate.Record, error) {
	updated, err := reviewstate.ApplyPush(r, pusher, sha)
	if err != nil {
		return r, err
	}
	return reviewstate.PassToken(updated), nil
}
