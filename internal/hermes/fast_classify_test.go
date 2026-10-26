package hermes

import "testing"

func TestFastClassifyGreetings(t *testing.T) {
	cases := []struct {
		input   string
		wantRes fastClassifyResult
		wantKey string
	}{
		{"Hi there!", fastChat, ""},
		{"Hello Jarvis", fastChat, ""},
		{"Hey, how are you?", fastChat, ""},
		{"Good morning!", fastChat, ""},
		{"Good night Jarvis", fastChat, ""},
		{"Sup!", fastChat, ""},
		{"Greetings!", fastChat, ""},
		// Greeting + action keyword → action
		{"Hey, what's the status?", fastAction, "check_status"},
		{"Hi, can you check status?", fastAction, "check_status"},
	}

	for _, tc := range cases {
		got, key := fastClassify(tc.input)
		if got != tc.wantRes {
			t.Errorf("fastClassify(%q) = %v, want %v", tc.input, got, tc.wantRes)
		}
		if key != tc.wantKey {
			t.Errorf("fastClassify(%q) key = %q, want %q", tc.input, key, tc.wantKey)
		}
	}
}

func TestFastClassifyQuestions(t *testing.T) {
	cases := []struct {
		input   string
		wantRes fastClassifyResult
	}{
		{"What is the capital of France?", fastChat},
		{"How does this work?", fastChat},
		{"Can you explain Go generics?", fastChat},
		{"What are you working on?", fastAction}, // check_status pattern
		{"What's going on?", fastAction},         // check_status pattern
	}

	for _, tc := range cases {
		got, _ := fastClassify(tc.input)
		if got != tc.wantRes {
			t.Errorf("fastClassify(%q) = %v, want %v", tc.input, got, tc.wantRes)
		}
	}
}

func TestFastClassifyActions(t *testing.T) {
	cases := []struct {
		input   string
		wantRes fastClassifyResult
		wantKey string
	}{
		// Status
		{"check status", fastAction, "check_status"},
		{"what's the status", fastAction, "check_status"},
		{"how's it going?", fastAction, "check_status"},
		{"what are you doing", fastAction, "check_status"},
		{"progress update", fastAction, "check_status"},
		// Jobs
		{"list jobs", fastAction, "list_jobs"},
		{"show me the tasks", fastAction, "list_jobs"},
		{"what's on the list", fastAction, "list_jobs"},
		// Start engine
		{"start the engine", fastAction, "start_engine"},
		{"resume work", fastAction, "start_engine"},
		// Stop engine
		{"stop the engine", fastAction, "stop_engine"},
		{"pause building", fastAction, "stop_engine"},
	}

	for _, tc := range cases {
		got, key := fastClassify(tc.input)
		if got != tc.wantRes {
			t.Errorf("fastClassify(%q) = %v, want %v", tc.input, got, tc.wantRes)
		}
		if key != tc.wantKey {
			t.Errorf("fastClassify(%q) key = %q, want %q", tc.input, key, tc.wantKey)
		}
	}
}

func TestFastClassifyStatements(t *testing.T) {
	cases := []struct {
		input   string
		wantRes fastClassifyResult
	}{
		// Short statements → chat
		{"I like coding in Go", fastChat},
		{"The weather is nice today", fastChat},
		{"Thanks for your help", fastChat},
		{"That makes sense", fastChat},
		{"I want to learn more about AI", fastChat},
		// Longer ambiguous messages → unknown (LLM)
		{"I was thinking about how we could improve the build pipeline by adding more parallel workers and maybe integrating with a CI system that tracks metrics over time and gives us better visibility", fastUnknown},
	}

	for _, tc := range cases {
		got, _ := fastClassify(tc.input)
		if got != tc.wantRes {
			t.Errorf("fastClassify(%q) = %v, want %v", tc.input, got, tc.wantRes)
		}
	}
}

func TestFastClassifyEmpty(t *testing.T) {
	cases := []string{"", "   ", "\t\n"}
	for _, input := range cases {
		got, _ := fastClassify(input)
		if got != fastUnknown {
			t.Errorf("fastClassify(%q) = %v, want %v (fastUnknown)", input, got, fastUnknown)
		}
	}
}

func TestFastClassifyCaseInsensitive(t *testing.T) {
	cases := []struct {
		input   string
		wantRes fastClassifyResult
	}{
		{"CHECK STATUS", fastAction},
		{"Check Status", fastAction},
		{"LIST JOBS", fastAction},
		{"Hey!", fastChat},
		{"HEY!", fastChat},
		{"WHAT IS 2+2?", fastChat},
	}

	for _, tc := range cases {
		got, _ := fastClassify(tc.input)
		if got != tc.wantRes {
			t.Errorf("fastClassify(%q) = %v, want %v", tc.input, got, tc.wantRes)
		}
	}
}
