package reviewauto

import "testing"

func TestOpenCap(t *testing.T) {
	t.Run("CanOpen", func(t *testing.T) {
		cases := []struct {
			name     string
			openPRs  int
			cap      int
			fullAuto bool
			want     bool
		}{
			{"none open under cap", 0, 2, true, true},
			{"at cap", 2, 2, true, false},
			{"over cap", 3, 2, true, false},
			{"cap does not apply when full auto off", 5, 2, false, true},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				if got := CanOpen(c.openPRs, c.cap, c.fullAuto); got != c.want {
					t.Errorf("CanOpen(%d, %d, %v) = %v, want %v",
						c.openPRs, c.cap, c.fullAuto, got, c.want)
				}
			})
		}
	})

	t.Run("CanStart", func(t *testing.T) {
		cases := []struct {
			name     string
			opensPR  bool
			openPRs  int
			cap      int
			fullAuto bool
			want     bool
		}{
			{"opens a PR under cap", true, 0, 2, true, true},
			{"opens a PR at cap", true, 2, 2, true, false},
			{"opens nothing at cap still runs", false, 2, 2, true, true},
			{"opens nothing over cap still runs", false, 5, 2, true, true},
			{"opens a PR with full auto off", true, 5, 2, false, true},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				if got := CanStart(c.opensPR, c.openPRs, c.cap, c.fullAuto); got != c.want {
					t.Errorf("CanStart(%v, %d, %d, %v) = %v, want %v",
						c.opensPR, c.openPRs, c.cap, c.fullAuto, got, c.want)
				}
			})
		}
	})
}
