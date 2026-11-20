// Package reviewauto holds the limits that keep NoBrakes full auto
// inside its guardrails (Jobs 9a-9c of docs/pr-review-build-plan.md):
// how many pull requests may be open at once, how much cloud review
// spend is allowed in a calendar month, and the risk rating a diff
// carries. Every function here is pure — the caller reads the caps
// and path lists from config/review.yaml and passes them in, and the
// caller sends any notice this package builds.
package reviewauto

// CanOpen reports whether full auto may open another pull request
// while openPRs of them are already open. The cap (max_open_prs in
// config/review.yaml) applies only while full auto is on: when it is
// off, the owner is at the keyboard and the cap does not apply.
func CanOpen(openPRs int, cap int, fullAuto bool) bool {
	if !fullAuto {
		return true
	}
	return openPRs < cap
}

// CanStart reports whether the agent-run loop may start a job whose
// result is (opensPR) a new pull request. A job that opens nothing
// still runs at the cap; a job that would open a pull request is
// held while CanOpen is false.
func CanStart(opensPR bool, openPRs int, cap int, fullAuto bool) bool {
	if !opensPR {
		return true
	}
	return CanOpen(openPRs, cap, fullAuto)
}
