package hermes

import (
	"regexp"
	"strings"
)

// fastClassifyResult is the outcome of a keyword-based classification.
type fastClassifyResult int

const (
	// fastUnknown means the message is ambiguous and needs LLM classification.
	fastUnknown fastClassifyResult = iota
	// fastChat means the message is clearly conversational.
	fastChat
	// fastAction means the message is clearly an action request.
	fastAction
)

// greetingRe matches common greetings at the start of a message.
var greetingRe = regexp.MustCompile(`(?i)^(hi|hello|hey|howdy|sup|yo|greetings|good\s+(morning|afternoon|evening|night))\b`)

// questionWords matches question starters.
var questionWords = regexp.MustCompile(`(?i)^\s*(what|who|when|where|why|how|is|are|do|does|did|can|could|would|should|will|would|there\s+(is|are)|i\s+(think|wonder|guess|believe|want|need|like))\b`)

// Mode-switch patterns (set_automode / set_fullauto). The nouns are matched
// as words so "automation" and "autobahn" cannot claim a message; full auto
// is checked first because it also contains the substring "auto".
var (
	fullAutoNounRe = regexp.MustCompile(`\bfull[- ]?auto\b`)
	autoModeNounRe = regexp.MustCompile(`\b(auto[- ]?mode|automode)\b`)
	// changeVerbRe: a word that makes a mode mention an explicit change
	// request. Without one, "full auto is on" / "is full auto on?" is a
	// statement or question, not a switch.
	changeVerbRe = regexp.MustCompile(`\b(turn|enable|disable|set|switch|put|make|go|start|stop)\b`)
	// onStateRe / offStateRe: the requested state. \b keeps "off" from
	// matching the "on" inside it.
	onStateRe  = regexp.MustCompile(`\b(on|enabled?|1)\b`)
	offStateRe = regexp.MustCompile(`\b(off|disabled?|0)\b`)
	// negationRe: "don't turn on full auto" is advice, not a request —
	// ambiguous enough that the LLM should read it.
	negationRe = regexp.MustCompile(`\b(don'?t|do\s+not|never|no|without)\b`)
)

// actionPattern pairs an action key with its matching regex.
type actionPattern struct {
	key string
	re  *regexp.Regexp
}

// actionPatterns lists action patterns in priority order.
// More specific patterns go first to avoid false matches.
var actionPatterns = []actionPattern{
	{
		key: "list_jobs",
		re:  regexp.MustCompile(`(?i)(list\s+(jobs|tasks|kanban)|show\s+(me\s+)?(the\s+)?(jobs|tasks|kanban)|what'?s\s+on\s+the\s+list|kanban|task\s+list|pending\s+(jobs|tasks))`),
	},
	{
		key: "check_status",
		re:  regexp.MustCompile(`(?i)(check\s+(the\s+)?status|what'?s\s+(going\s+)?on|what'?s\s+the\s+status|status\s+(update|check)|how'?s\s+(it\s+)?going|progress\s+(report|update)|what are you working on|what are you doing|current\s+(job|task)|busy\s+(with|doing)?)`),
	},
	{
		key: "start_engine",
		re:  regexp.MustCompile(`(?i)(start\s+(up\s+)?(the\s+)?engine|enable\s+engine|turn\s+on\s+(the\s+)?engine|boot\s+(up\s+)?engine|resume\s+(work|building))`),
	},
	{
		key: "stop_engine",
		re:  regexp.MustCompile(`(?i)(stop\s+(the\s+)?engine|disable\s+engine|shut\s+down\s+(the\s+)?engine|pause\s+(work|building)|turn\s+off\s+(the\s+)?engine)`),
	},
}

// fastClassify attempts to classify a message using keyword patterns.
// It returns fastChat, fastAction (with the action name and, for the mode
// switches, the requested state in params["mode"]), or fastUnknown when the
// message is ambiguous and needs LLM classification.
//
// This avoids an LLM call for ~80% of messages, saving 3-5 seconds.
func fastClassify(content string) (fastClassifyResult, string, map[string]string) {
	lower := strings.ToLower(strings.TrimSpace(content))

	// Empty or whitespace-only messages are not classified.
	if lower == "" {
		return fastUnknown, "", nil
	}

	// Word count: long messages are ambiguous — let the LLM decide.
	// Check this first so action keyword patterns don't false-match
	// on words that appear separately in a long sentence.
	wordCount := strings.Count(lower, " ") + 1
	if wordCount >= 30 {
		return fastUnknown, "", nil
	}

	// 1. Greetings → chat
	if greetingRe.MatchString(lower) {
		// "Hey, can you check status?" — greeting + action keyword = action
		if actionMatch := matchActionPatterns(lower); actionMatch != "" {
			return fastAction, actionMatch, nil
		}
		if key, mode := matchModeSwitch(lower); key != "" {
			return modeResult(key, mode)
		}
		return fastChat, "", nil
	}

	// 2. Questions → chat (unless they ask about status/jobs or are a
	//    request in question form: "can you turn on full auto?")
	if strings.HasSuffix(lower, "?") || questionWords.MatchString(lower) {
		if actionMatch := matchActionPatterns(lower); actionMatch != "" {
			return fastAction, actionMatch, nil
		}
		if key, mode := matchModeSwitch(lower); key != "" {
			return modeResult(key, mode)
		}
		return fastChat, "", nil
	}

	// 3. Mode switches → action with the requested state (or LLM when the
	//    state is ambiguous). Checked before the generic patterns: the mode
	//    nouns are the more specific signal than the engine verbs.
	if key, mode := matchModeSwitch(lower); key != "" {
		return modeResult(key, mode)
	}

	// 4. Action keywords → action
	if actionMatch := matchActionPatterns(lower); actionMatch != "" {
		return fastAction, actionMatch, nil
	}

	// 5. Short statements without action keywords → chat
	return fastChat, "", nil
}

// modeResult packages a mode-switch match: an explicit state is a fast
// action with params["mode"] set; a named-but-ambiguous mode (no state,
// negated, a state question) goes to the LLM, which reads the sentence.
func modeResult(key, mode string) (fastClassifyResult, string, map[string]string) {
	if mode == "" {
		return fastUnknown, "", nil
	}
	return fastAction, key, modeParams(mode)
}

// modeParams packages the requested state for the mode-switch actions.
func modeParams(mode string) map[string]string {
	return map[string]string{"mode": mode}
}

// matchModeSwitch detects a request to change the NoBrakes auto_mode /
// full_auto switch. The message must name the switch; the requested state
// is explicit only when exactly one of on/off appears AND a change verb is
// present AND nothing is negated. When the mode is named but the state is
// ambiguous (no state — "go full auto"; both states — "is it on or off?";
// no verb — "full auto is on"; a negation — "don't turn it on"), it returns
// the key with an empty mode, and the caller sends it to the LLM, which
// reads the whole sentence. A question must never flip a flag: the LLM
// classifies questions as chat.
// Returns the action key ("" when no mode is named) and the requested
// state ("on"/"off", or "" when ambiguous).
func matchModeSwitch(lower string) (key, mode string) {
	switch {
	case fullAutoNounRe.MatchString(lower):
		key = "set_fullauto"
	case autoModeNounRe.MatchString(lower):
		key = "set_automode"
	default:
		return "", ""
	}
	on, off := onStateRe.MatchString(lower), offStateRe.MatchString(lower)
	if on == off || negationRe.MatchString(lower) || !changeVerbRe.MatchString(lower) {
		return key, "" // ambiguous — the LLM decides
	}
	if on {
		return key, "on"
	}
	return key, "off"
}

// matchActionPatterns checks the message against registered action patterns.
// Returns the action key if matched, or empty string.
// Patterns are checked in order; the first match wins.
func matchActionPatterns(lower string) string {
	for _, p := range actionPatterns {
		if p.re.MatchString(lower) {
			return p.key
		}
	}
	return ""
}
