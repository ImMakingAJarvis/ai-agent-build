package hermes

import "testing"

// classify3 is fastClassify with the params return unwrapped, for table
// tests that only care about result and action key.
func classify3(t *testing.T, input string) (fastClassifyResult, string, map[string]string) {
	t.Helper()
	res, key, params := fastClassify(input)
	if key == "" && params != nil {
		t.Fatalf("fastClassify(%q): params without action key", input)
	}
	return res, key, params
}

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
		got, key, _ := classify3(t, tc.input)
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
		// A state question names the switch but asks — it must not flip a
		// flag; it goes to the LLM, which classifies questions as chat.
		{"Is full auto on?", fastUnknown},
		{"Is auto mode on or off?", fastUnknown},
	}

	for _, tc := range cases {
		got, _, _ := classify3(t, tc.input)
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
		// Mode words must not claim the engine patterns and vice versa
		{"turn on the engine", fastAction, "start_engine"},
	}

	for _, tc := range cases {
		got, key, _ := classify3(t, tc.input)
		if got != tc.wantRes {
			t.Errorf("fastClassify(%q) = %v, want %v", tc.input, got, tc.wantRes)
		}
		if key != tc.wantKey {
			t.Errorf("fastClassify(%q) key = %q, want %q", tc.input, key, tc.wantKey)
		}
	}
}

func TestFastClassifyModeSwitches(t *testing.T) {
	cases := []struct {
		input   string
		wantRes fastClassifyResult
		wantKey string
		wantMode string
	}{
		// Full auto
		{"turn on full auto", fastAction, "set_fullauto", "on"},
		{"turn full auto on", fastAction, "set_fullauto", "on"},
		{"Turn ON Full Auto", fastAction, "set_fullauto", "on"}, // case-insensitive
		{"turn off full auto", fastAction, "set_fullauto", "off"},
		{"turn full auto off", fastAction, "set_fullauto", "off"},
		{"enable full auto", fastAction, "set_fullauto", "on"},
		{"disable full auto", fastAction, "set_fullauto", "off"},
		{"set full auto to off", fastAction, "set_fullauto", "off"},
		{"put it on full auto", fastAction, "set_fullauto", "on"},
		{"switch to full auto, on", fastAction, "set_fullauto", "on"},
		// Auto mode (the nightly batch)
		{"turn on auto mode", fastAction, "set_automode", "on"},
		{"turn off auto mode", fastAction, "set_automode", "off"},
		{"enable auto mode", fastAction, "set_automode", "on"},
		{"disable auto mode", fastAction, "set_automode", "off"},
		{"turn on automode", fastAction, "set_automode", "on"},
		// Requests in question form carry the state and the verb → action
		{"can you turn on full auto?", fastAction, "set_fullauto", "on"},
		{"could you turn off auto mode?", fastAction, "set_automode", "off"},
		// Greeting + request → action
		{"hey, turn on full auto", fastAction, "set_fullauto", "on"},
		// Ambiguous: mode named but the state is not explicit → LLM
		{"go full auto", fastUnknown, "", ""},
		{"switch to full auto", fastUnknown, "", ""},
		{"full auto is on", fastUnknown, "", ""}, // statement, no verb
		{"don't turn on full auto", fastUnknown, "", ""}, // negated
		// "automation" / other auto-words must not claim the switch
		{"what is automation", fastChat, "", ""},
	}

	for _, tc := range cases {
		got, key, params := classify3(t, tc.input)
		if got != tc.wantRes {
			t.Errorf("fastClassify(%q) = %v, want %v", tc.input, got, tc.wantRes)
			continue
		}
		if key != tc.wantKey {
			t.Errorf("fastClassify(%q) key = %q, want %q", tc.input, key, tc.wantKey)
		}
		if got == fastAction {
			if params["mode"] != tc.wantMode {
				t.Errorf("fastClassify(%q) mode = %q, want %q", tc.input, params["mode"], tc.wantMode)
			}
		} else if params != nil {
			t.Errorf("fastClassify(%q) unexpected params %v", tc.input, params)
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
		got, _, _ := classify3(t, tc.input)
		if got != tc.wantRes {
			t.Errorf("fastClassify(%q) = %v, want %v", tc.input, got, tc.wantRes)
		}
	}
}

func TestFastClassifyCapabilityWork(t *testing.T) {
	cases := []struct {
		input   string
		wantRes fastClassifyResult
	}{
		// Capability work: the fast path cannot extract the parameters
		// (capability, path), so it defers to the LLM — the spec's
		// acceptance message must reach submit_capability.
		{"describe the photos in folder /tmp/photos", fastUnknown},
		{"please describe these photos", fastUnknown},
		{"transcribe the audio in /tmp/recordings", fastUnknown},
		{"Generate a picture of a cat", fastUnknown},
		{"caption the screenshots in the build folder", fastUnknown},
		// Control: ordinary short statements still go straight to chat.
		{"I like coding in Go", fastChat},
		{"That makes sense", fastChat},
	}

	for _, tc := range cases {
		got, _, _ := classify3(t, tc.input)
		if got != tc.wantRes {
			t.Errorf("fastClassify(%q) = %v, want %v", tc.input, got, tc.wantRes)
		}
	}
}

func TestFastClassifyEmpty(t *testing.T) {
	cases := []string{"", "   ", "\t\n"}
	for _, input := range cases {
		got, _, _ := classify3(t, input)
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
		{"TURN OFF FULL AUTO", fastAction},
	}

	for _, tc := range cases {
		got, _, _ := classify3(t, tc.input)
		if got != tc.wantRes {
			t.Errorf("fastClassify(%q) = %v, want %v", tc.input, got, tc.wantRes)
		}
	}
}
