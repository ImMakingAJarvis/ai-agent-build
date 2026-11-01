package planeepic

import "testing"

func TestSanitizeProjectName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Test", "Test"},
		{"Full e2e test and fix of voice chat.", "Full e2e test and fix of voice chat"},
		{"Work board: Plane + Plan with model", "Work board Plane Plan with model"},
		{"Own voice channel: camera mics + direct Sonos/Google speakers (no Home Assistant)",
			"Own voice channel camera mics direct Sonos/Google speakers no Home Assistant"},
		{"NoBrakes & friends", "NoBrakes friends"},
		{"a - b", "a b"},
		{"  spaced   out  ", "spaced out"},
	}
	for _, tc := range cases {
		if got := SanitizeProjectName(tc.in); got != tc.want {
			t.Errorf("SanitizeProjectName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
