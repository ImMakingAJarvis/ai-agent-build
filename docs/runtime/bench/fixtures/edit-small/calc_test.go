package calc

import "testing"

func TestAverage(t *testing.T) {
	if got := Average([]float64{2, 4, 6}); got != 4 {
		t.Fatalf("Average = %v, want 4", got)
	}
	if got := Average(nil); got != 0 {
		t.Fatalf("Average(nil) = %v, want 0", got)
	}
}
