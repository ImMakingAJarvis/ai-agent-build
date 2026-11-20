package reviewstate

import "fmt"

// ApplyPush applies a push of newSHA by pusher. Only the model holding the
// turn token may push. A push sets that model's stamp to the new SHA,
// clears the other model's stamp (the other model has not confirmed the
// new head), moves Head, and marks the turn as pushed. Opening a pull
// request is not a push: the caller sets AuthorStamp and Head to the same
// SHA when the record is first saved, because the author wrote that
// commit.
func ApplyPush(r Record, pusher, newSHA string) (Record, error) {
	if pusher != r.Token {
		return r, fmt.Errorf("reviewstate: pusher %q does not hold the turn token %q", pusher, r.Token)
	}
	switch pusher {
	case r.AuthorModel:
		r.AuthorStamp = newSHA
		r.ReviewerStamp = ""
	case r.ReviewerModel:
		r.ReviewerStamp = newSHA
		r.AuthorStamp = ""
	default:
		return r, fmt.Errorf("reviewstate: pusher %q is neither the author %q nor the reviewer %q",
			pusher, r.AuthorModel, r.ReviewerModel)
	}
	r.Head = newSHA
	r.TurnPushed = true
	return r, nil
}

// ApplyStamp is the no-push LGTM: the model holding the turn token
// confirms the current head without pushing. A turn that already pushed
// cannot also stamp, and only the current head can be stamped.
func ApplyStamp(r Record, who, sha string) (Record, error) {
	if who != r.Token {
		return r, fmt.Errorf("reviewstate: %q does not hold the turn token %q", who, r.Token)
	}
	if r.TurnPushed {
		return r, fmt.Errorf("reviewstate: this turn already pushed, so it cannot also stamp")
	}
	if sha != r.Head {
		return r, fmt.Errorf("reviewstate: stamp sha %q is not the current head %q", sha, r.Head)
	}
	switch who {
	case r.AuthorModel:
		r.AuthorStamp = sha
	case r.ReviewerModel:
		r.ReviewerStamp = sha
	default:
		return r, fmt.Errorf("reviewstate: %q is neither the author %q nor the reviewer %q",
			who, r.AuthorModel, r.ReviewerModel)
	}
	return r, nil
}

// PassToken ends the turn: TurnPushed is cleared and the token goes to the
// other of the two model ids.
func PassToken(r Record) Record {
	r.TurnPushed = false
	if r.Token == r.AuthorModel {
		r.Token = r.ReviewerModel
	} else {
		r.Token = r.AuthorModel
	}
	return r
}
