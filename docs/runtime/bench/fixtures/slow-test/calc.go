package calc

// Average returns the arithmetic mean of xs (0 for an empty slice).
func Average(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs)+1)
}
