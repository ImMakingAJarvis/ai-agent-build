package reviewauto

import "strings"

// authorRatings are the author ratings, lowest to highest. "note"
// may merge with no reviewer; every other rating waits for review.
// An author rating outside this list (including empty) rates high, so
// a bad stamp can never skip review.
var authorRatings = []string{"note", "medium-low", "medium", "high", "very high"}

// ratingRank is the position of rating in authorRatings, or -1 when
// it is not one of them.
func ratingRank(r string) int {
	for i, a := range authorRatings {
		if a == r {
			return i
		}
	}
	return -1
}

// Rate returns the risk rating of a diff: the author's rating,
// floored by the sensitive paths — a file on the list makes the diff
// high even if the author said note, and never lowers a higher
// rating. An unknown or empty author rating is high.
//
// The sensitive list comes from config/review.yaml (the
// SensitivePaths of reviewcfg.Config); this package keeps no copy of
// it, and the caller passes it in.
func Rate(authorRating string, files, sensitive []string) string {
	r := authorRating
	if ratingRank(r) < 0 {
		r = "high"
	}
	if touchesSensitive(files, sensitive) && ratingRank(r) < ratingRank("high") {
		return "high"
	}
	return r
}

// NeedsReview reports whether a rating waits for a reviewer: every
// rating except "note".
func NeedsReview(rating string) bool {
	return rating != "note"
}

// touchesSensitive reports whether any file equals a sensitive entry
// or, for an entry that ends in "/", lies under it. A file entry
// such as docker-compose.yml matches only that file; an entry such
// as cmd/resourced/ matches every file under it.
func touchesSensitive(files, sensitive []string) bool {
	for _, f := range files {
		for _, s := range sensitive {
			switch {
			case strings.HasSuffix(s, "/"):
				if strings.HasPrefix(f, s) {
					return true
				}
			case f == s:
				return true
			}
		}
	}
	return false
}
