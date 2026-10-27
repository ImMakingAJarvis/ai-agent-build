// Runtime band order for the agent job queue.
//
// The band (class) priority order used to be a static map: fix, then build,
// then background. It is now mutable at runtime and persisted in
// band_order.json in the store directory. The dashboard exposes it through
// GET/POST /api/queue/band-order (settings UI) and the CLI through
// `jarvis-jobs order`; the run order (Store.List / Store.Next) and the
// drag-and-drop queue both follow it.
package agentjob

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// bandOrderFile holds the runtime band order, next to the jobs.
const bandOrderFile = "band_order.json"

// DefaultBandOrder is the built-in band priority (highest first): fix, build,
// background. A store without a band_order.json file sorts by this.
func DefaultBandOrder() []string {
	return []string{ClassFix, ClassBuild, ClassBackground}
}

// bandOrderPath is where the runtime band order lives.
func (s *Store) bandOrderPath() string {
	return filepath.Join(s.dir, bandOrderFile)
}

// BandOrder returns the current runtime band order (highest priority first).
// A missing or corrupt file falls back to DefaultBandOrder so a bad file
// can never wedge the queue.
func (s *Store) BandOrder() ([]string, error) {
	b, err := os.ReadFile(s.bandOrderPath())
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultBandOrder(), nil
		}
		return nil, err
	}
	var order []string
	if err := json.Unmarshal(b, &order); err != nil || !isBandPermutation(order) {
		return DefaultBandOrder(), nil
	}
	return order, nil
}

// SetBandOrder persists a new runtime band order (highest priority first).
// order must be a permutation of the three job classes; anything else is
// rejected and the stored order is left unchanged.
func (s *Store) SetBandOrder(order []string) error {
	if !isBandPermutation(order) {
		return fmt.Errorf("agentjob: band order %v must list %v exactly once each",
			order, DefaultBandOrder())
	}
	return writeJSON(s.bandOrderPath(), order)
}

// isBandPermutation reports whether order names each job class exactly once.
func isBandPermutation(order []string) bool {
	if len(order) != len(DefaultBandOrder()) {
		return false
	}
	seen := make(map[string]bool, len(order))
	for _, c := range order {
		if seen[c] || !isClass(c) {
			return false
		}
		seen[c] = true
	}
	return true
}

// isClass reports whether c is one of the known job classes.
func isClass(c string) bool {
	switch c {
	case ClassFix, ClassBuild, ClassBackground:
		return true
	}
	return false
}

// bandRank maps each class to its rank (0 = highest priority) under the
// current runtime band order. List uses it so every caller sees one order.
func (s *Store) bandRank() (map[string]int, error) {
	order, err := s.BandOrder()
	if err != nil {
		return nil, err
	}
	rank := make(map[string]int, len(order))
	for i, b := range order {
		rank[b] = i
	}
	return rank, nil
}

// Rank returns the band rank (0 = highest priority) of a job's effective
// class under the current runtime band order.
func (s *Store) Rank(class, source string) (int, error) {
	rank, err := s.bandRank()
	if err != nil {
		return 0, err
	}
	return rank[EffectiveClass(class, source)], nil
}
