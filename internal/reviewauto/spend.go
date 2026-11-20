package reviewauto

import (
	"fmt"
	"time"
)

// AddSpend returns prev plus the cost of tokens at usdPerMTok per
// million tokens (prices_usd_per_mtok in config/review.yaml).
func AddSpend(prev float64, tokens int, usdPerMTok float64) float64 {
	return prev + float64(tokens)/1e6*usdPerMTok
}

// OverCap reports whether the month's spend has reached the cap
// (month_cap_usd in config/review.yaml). At the cap the caller does
// not call RequestReview: the pull request stays waiting.
func OverCap(spent, cap float64) bool {
	return spent >= cap
}

// MonthKey is the calendar month of t in America/Los_Angeles as
// YYYY-MM — the month a review's spend is counted against.
func MonthKey(t time.Time) string {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		// No zoneinfo on this host; fall back to fixed UTC-8.
		// Wrong by an hour in July, but this path should not happen.
		la = time.FixedZone("LA", -8*3600)
	}
	return t.In(la).Format("2006-01")
}

// Notice is the two texts the caller sends when a review is blocked:
// Plane is posted on the pull request's Plane item, Discord is one
// line to Nathan. This package never sends anything.
type Notice struct {
	Plane   string
	Discord string
}

// SpendBlocked is the notice for a pull request whose review was not
// requested because the month's cloud review spend reached the cap.
func SpendBlocked(planeItem string) Notice {
	return Notice{
		Plane: fmt.Sprintf(
			"%s: review not requested — the month's cloud review spend has reached the $10 cap; the pull request stays open and waiting.",
			planeItem),
		Discord: fmt.Sprintf(
			"%s is waiting on review: the month's review spend hit the $10 cap.",
			planeItem),
	}
}
