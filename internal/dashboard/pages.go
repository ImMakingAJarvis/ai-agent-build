package dashboard

import (
	"net/http"
	"strconv"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// First-paint page size for unbounded dashboard lists.
const listPageSize = 25

// ListPage is one window over a larger on-disk list. History is not deleted.
type ListPage struct {
	Offset     int
	Limit      int
	Total      int
	Shown      int
	NextOffset int
	HasMore    bool
	Hidden     int
}

func parseOffset(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if n < 0 {
		return 0
	}
	return n
}

func window[T any](all []T, offset, limit int) (page []T, info ListPage) {
	if limit <= 0 {
		limit = listPageSize
	}
	if offset < 0 {
		offset = 0
	}
	total := len(all)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page = all[offset:end]
	info = ListPage{
		Offset:     offset,
		Limit:      limit,
		Total:      total,
		Shown:      end,
		NextOffset: end,
		HasMore:    end < total,
		Hidden:     total - end,
	}
	return page, info
}

func slimAgentJob(j agentjob.Job) agentjob.Job {
	j.Instructions = ""
	j.Summary = ""
	return j
}

func rankAgentStatus(status string) int {
	switch status {
	case agentjob.Running:
		return 0
	case agentjob.Claimed:
		return 1
	case agentjob.Ready:
		return 2
	case agentjob.Paused:
		return 3
	case agentjob.Failed:
		return 4
	default:
		return 5
	}
}

func orderAgentJobs(jobs []agentjob.Job) []agentjob.Job {
	out := make([]agentjob.Job, len(jobs))
	copy(out, jobs)
	for i := range out {
		out[i] = slimAgentJob(out[i])
	}
	// insertion sort is fine for a few hundred rows
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && agentLess(out[j], out[j-1]) {
			out[j], out[j-1] = out[j-1], out[j]
			j--
		}
	}
	return out
}

func agentLess(a, b agentjob.Job) bool {
	ra, rb := rankAgentStatus(a.Status), rankAgentStatus(b.Status)
	if ra != rb {
		return ra < rb
	}
	if a.StartedAt != b.StartedAt {
		return a.StartedAt > b.StartedAt
	}
	return a.ID < b.ID
}
