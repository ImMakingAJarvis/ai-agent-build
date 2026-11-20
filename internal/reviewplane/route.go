package reviewplane

// Comment is a Plane work-item comment reduced to what routing needs
// (Job 11b of docs/pr-review-build-plan.md). AuthorID is the commenter's
// workspace member id (plane.Comment.CreatedBy); ParentAuthorID is the
// author of the comment being replied to, empty if it is not a reply; Body
// is the comment text.
type Comment struct {
	AuthorID       string
	ParentAuthorID string
	Body           string
}

// Route decides which model a Plane comment is for. userModels maps a
// Plane user id to the model id that owns it; Nathan is not in the map.
// authorModel is the model the comment's author user id belongs to, and
// namedModel is the model the body names ("" if it names none), found by
// the caller.
//
// A reply to a known model goes to that model, not guessed. A body that
// names a model other than its author goes to that model, not guessed.
// Everything else — including a reply to Nathan and a body that names the
// author itself — goes to the author model with guessed true: the author
// must say, in its Plane reply, who it decided the comment was for.
func Route(c Comment, userModels map[string]string, authorModel, namedModel string) (target string, guessed bool) {
	if c.ParentAuthorID != "" {
		if m, ok := userModels[c.ParentAuthorID]; ok && m != "" {
			return m, false
		}
	}
	if namedModel != "" && namedModel != authorModel {
		return namedModel, false
	}
	return authorModel, true
}
