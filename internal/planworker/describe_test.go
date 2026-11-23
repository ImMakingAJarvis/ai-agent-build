package planworker

import (
	"strings"
	"testing"
)

func TestCheckDescriptions(t *testing.T) {
	long := "Add internal/reviewstate/store.go with a Store type that keeps one JSON file per pull request " +
		"under a directory the caller passes in. Save(rec) writes atomically (temp file + rename); " +
		"Load(n) returns os.ErrNotExist for an unknown PR. TestStoreRoundTrip covers both."
	cases := []struct {
		name string
		task Task
		want string // substring of the error; "" = valid
	}{
		{"self-contained", Task{ID: "a", Description: long}, ""},
		{"too short", Task{ID: "a", Description: "Add the store."}, "at least 200"},
		{"job pointer", Task{ID: "a", Description: "Job 2a of docs/pr-review-build-plan.md."}, `task "a"`},
		{"padded pointer", Task{ID: "a", Description: "Build the file store for one pull request exactly " +
			"as it is written down for this step, including every edge case and test that is named there. " +
			"See the plan, section 2a, for the details of the record format and the directory layout."}, "refers to the plan"},
		{"long text may cite the plan", Task{ID: "a", Description: long + " " + long + " See the plan, section 2a, for the background."}, ""},
		{"capability input", Task{ID: "a", Capability: "image.generate", Description: "a red bicycle on a beach at dusk"}, ""},
		{"capability empty", Task{ID: "a", Capability: "image.generate", Description: "a bike"}, "full input"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckDescriptions(TaskList{Tasks: []Task{c.task}})
			if c.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}
