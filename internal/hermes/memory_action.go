package hermes

import (
	"context"
	"fmt"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/memory"
)

// memoryResultCap keeps action results inside Discord's message budget.
const memoryResultCap = 1800

// MemoryAction builds the "search_memory" action: a semantic lookup over
// the local memory service (work logs, job transcripts, job specs, pending
// queue, kanban, repo docs). The classifier extracts a "query" parameter.
func MemoryAction(mem *memory.Client) Action {
	return Action{
		Key:         "search_memory",
		Description: "Search the work logs, job history and pending queue for context (\"query\" parameter)",
		Run: func(ctx context.Context, params map[string]string) (string, error) {
			query := strings.TrimSpace(params["query"])
			if query == "" {
				return "", fmt.Errorf("search_memory: query parameter is required")
			}
			hits := mem.Search(ctx, query, 5)
			if hits == nil {
				return "The memory service is not reachable right now; try again in a moment.", nil
			}
			if len(hits) == 0 {
				return fmt.Sprintf("No matches for %q in the work logs, job history or queue.", query), nil
			}
			return fmt.Sprintf("Found %d matching note(s):\n%s",
				len(hits), memory.FormatContext(hits, memoryResultCap)), nil
		},
	}
}
