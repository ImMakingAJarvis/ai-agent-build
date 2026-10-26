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
// It returns fastChat, fastAction (with the action name), or fastUnknown
// when the message is ambiguous and needs LLM classification.
//
// This avoids an LLM call for ~80% of messages, saving 3-5 seconds.
func fastClassify(content string) (fastClassifyResult, string) {
	lower := strings.ToLower(strings.TrimSpace(content))

	// Empty or whitespace-only messages are not classified.
	if lower == "" {
		return fastUnknown, ""
	}

	// Word count: long messages are ambiguous — let the LLM decide.
	// Check this first so action keyword patterns don't false-match
	// on words that appear separately in a long sentence.
	wordCount := strings.Count(lower, " ") + 1
	if wordCount >= 30 {
		return fastUnknown, ""
	}

	// 1. Greetings → chat
	if greetingRe.MatchString(lower) {
		// "Hey, can you check status?" — greeting + action keyword = action
		if actionMatch := matchActionPatterns(lower); actionMatch != "" {
			return fastAction, actionMatch
		}
		return fastChat, ""
	}

	// 2. Questions → chat (unless they ask about status/jobs)
	if strings.HasSuffix(lower, "?") || questionWords.MatchString(lower) {
		if actionMatch := matchActionPatterns(lower); actionMatch != "" {
			return fastAction, actionMatch
		}
		return fastChat, ""
	}

	// 3. Action keywords → action
	if actionMatch := matchActionPatterns(lower); actionMatch != "" {
		return fastAction, actionMatch
	}

	// 4. Short statements without action keywords → chat
	return fastChat, ""
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
