package reviewauto

import (
	"strings"
	"testing"
	"time"
)

func TestSpend(t *testing.T) {
	t.Run("AddSpend", func(t *testing.T) {
		// 1_000_000 tokens at $3 per million is $3.
		if got := AddSpend(0, 1_000_000, 3); got != 3 {
			t.Errorf("AddSpend(0, 1_000_000, 3) = %v, want 3", got)
		}
		// 0 tokens adds 0.
		if got := AddSpend(7.5, 0, 3); got != 7.5 {
			t.Errorf("AddSpend(7.5, 0, 3) = %v, want 7.5", got)
		}
		// It accumulates onto the previous month's total.
		if got := AddSpend(1.5, 500_000, 3); got != 3 {
			t.Errorf("AddSpend(1.5, 500_000, 3) = %v, want 3", got)
		}
	})

	t.Run("OverCap", func(t *testing.T) {
		if !OverCap(10, 10) {
			t.Error("OverCap(10, 10) = false, want true")
		}
		if OverCap(9.99, 10) {
			t.Error("OverCap(9.99, 10) = true, want false")
		}
	})

	t.Run("MonthKey", func(t *testing.T) {
		// 07:30 UTC on 2026-01-01 is still 23:30 on December 31 in
		// Los Angeles, so the spend month is 2025-12.
		t1 := time.Date(2026, 1, 1, 7, 30, 0, 0, time.UTC)
		if got := MonthKey(t1); got != "2025-12" {
			t.Errorf("MonthKey(2026-01-01T07:30:00Z) = %q, want %q", got, "2025-12")
		}
		// Noon UTC in June is morning the same day in Los Angeles.
		t2 := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
		if got := MonthKey(t2); got != "2026-06" {
			t.Errorf("MonthKey(2026-06-15T12:00:00Z) = %q, want %q", got, "2026-06")
		}
	})

	t.Run("SpendBlocked", func(t *testing.T) {
		n := SpendBlocked("T2I-3")
		if !strings.Contains(n.Plane, "T2I-3") {
			t.Errorf("Plane text does not contain the item: %q", n.Plane)
		}
		if !strings.Contains(n.Plane, "10") {
			t.Errorf("Plane text does not contain the words 10: %q", n.Plane)
		}
		if strings.Contains(n.Discord, "\n") {
			t.Errorf("Discord text is not one line: %q", n.Discord)
		}
		if !strings.Contains(n.Discord, "T2I-3") {
			t.Errorf("Discord text does not name the item: %q", n.Discord)
		}
		if !strings.Contains(n.Discord, "10") {
			t.Errorf("Discord text does not carry the cap: %q", n.Discord)
		}
	})
}
