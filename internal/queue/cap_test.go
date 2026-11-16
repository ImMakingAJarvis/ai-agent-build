package queue

import (
	"testing"
	"time"
)

func TestValidCapabilityName(t *testing.T) {
	cases := map[string]bool{
		"echo":            true,
		"image.describe":  true,
		"audio.transcribe": true,
		"bench.run2":      true,
		"":                false,
		"Image.describe":  false, // uppercase
		"image.":          false, // trailing dot
		".describe":       false, // leading dot
		"image..describe": false, // empty segment
		"img_gen":         false, // underscore
		"image-describe":  false, // hyphen
		"image describe":  false, // space
	}
	for name, want := range cases {
		if got := ValidCapabilityName(name); got != want {
			t.Errorf("ValidCapabilityName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestParseDeadline(t *testing.T) {
	// Duration from now.
	tm, err := ParseDeadline("2h")
	if err != nil {
		t.Fatalf("ParseDeadline(2h): %v", err)
	}
	if d := time.Until(tm); d < 119*time.Minute || d > 121*time.Minute {
		t.Errorf("2h deadline = %v from now, want ~2h", d)
	}
	// Absolute RFC3339.
	tm2, err := ParseDeadline("2027-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("ParseDeadline(rfc3339): %v", err)
	}
	if tm2.Year() != 2027 {
		t.Errorf("rfc3339 deadline = %v, want 2027", tm2)
	}
	// Garbage.
	if _, err := ParseDeadline("soon"); err == nil {
		t.Error("ParseDeadline(soon): want error")
	}
	// Negative duration.
	if _, err := ParseDeadline("-5m"); err == nil {
		t.Error("ParseDeadline(-5m): want error")
	}
}
