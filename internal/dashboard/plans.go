// Package dashboard: plan uploads.
//
// The owner uploads a plan file (markdown or anything else) from the
// dashboard's Plans section. The dashboard stores the file under
// <dataDir>/plans/<plan-id>/ and enqueues a ready agent job with the same
// ID, whose instructions are the recipe for the worker's round: read the
// plan, break it into jobs, and create each one with `jarvis-jobs add`.
// The breakdown itself happens in the worker's round, not here — the
// dashboard only stores the file and enqueues the job.
package dashboard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

const (
	// planDefaultPriority places a plan job ahead of the ordinary build
	// jobs (priority 0) in the build band so the worker picks it up
	// promptly; the fix band still runs first (band order).
	planDefaultPriority = 10

	// planListMax is how many plans the list endpoints show, newest first.
	planListMax = 30
)

// maxPlanBytes caps the size of a single plan file (20 MB). It is a var so
// tests can shrink it.
var maxPlanBytes int64 = 20 << 20

// planIDRe matches the job IDs this package creates (plan-<slug>-<unix
// seconds>), so the list endpoints only report plans this dashboard made.
// The slug may contain hyphens; only the last dash separates the timestamp.
var planIDRe = regexp.MustCompile(`^plan-[a-z0-9][a-z0-9-]*-\d+$`)

// planSlugRe strips everything but lowercase alphanumerics out of a plan ID's
// slug.
var planSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// plansState is the dashboard's plan state: the plans directory.
type plansState struct {
	dir string // <dataDir>/plans
}

// initPlans points the plan state at the plans directory. The directory
// itself is created on the first upload, not at startup.
func initPlans(dataDir string) *plansState {
	return &plansState{dir: filepath.Join(dataDir, "plans")}
}

// planUploadResponse is the body of POST /api/plans.
type planUploadResponse struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	File      string `json:"file"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	JobID     string `json:"job_id"`
	JobStatus string `json:"job_status"`
}

// planInfo is one uploaded plan as shown by the list endpoints.
type planInfo struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	File       string `json:"file"`
	Size       int64  `json:"size"`
	UploadedAt string `json:"uploaded_at"`
	JobStatus  string `json:"job_status"`
}

// handlePlanUpload stores the uploaded plan file and enqueues a ready plan
// job for the worker. Multipart form fields:
//
//	file     the plan file (required)
//	title    plan name (default: the file name)
//	class    fix, build or background (default build)
//	priority priority within the class (default 10)
//
// The plan file and the job share one ID: <dataDir>/plans/<id>/<file> and
// the agent job <id>. The response is the created plan and its job.
func (ps *plansState) handlePlanUpload(s *Server, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, "bad multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "a file field is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Any directory part the browser included is dropped up front; the
	// on-disk name is then sanitized, while the displayed title keeps the
	// original spacing.
	baseName := filepath.Base(filepath.Clean("/" + header.Filename))
	name := sanitizePlanName(baseName)
	if name == "" {
		http.Error(w, "file name is not usable", http.StatusBadRequest)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = baseName
	}

	class := strings.TrimSpace(r.FormValue("class"))
	if class == "" {
		class = agentjob.ClassBuild
	}
	switch class {
	case agentjob.ClassFix, agentjob.ClassBuild, agentjob.ClassBackground:
	default:
		http.Error(w, "class must be fix, build or background", http.StatusBadRequest)
		return
	}

	priority := planDefaultPriority
	if p := strings.TrimSpace(r.FormValue("priority")); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			http.Error(w, "priority must be a non-negative number", http.StatusBadRequest)
			return
		}
		priority = n
	}

	planID := fmt.Sprintf("plan-%s-%d", planSlug(name), time.Now().Unix())

	st, err := s.openAgentJobStore()
	if err != nil {
		http.Error(w, "job store unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := st.Get(planID); err == nil {
		// Same file name uploaded within the same second.
		http.Error(w, "a plan with this name was just uploaded; wait a second and retry", http.StatusConflict)
		return
	}

	planDir := filepath.Join(ps.dir, planID)
	if err := os.MkdirAll(planDir, 0o755); err != nil {
		http.Error(w, "store plan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	path := filepath.Join(planDir, name)
	out, err := os.Create(path)
	if err != nil {
		os.RemoveAll(planDir)
		http.Error(w, "store plan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	n, err := io.Copy(out, io.LimitReader(file, maxPlanBytes+1))
	out.Close()
	if err != nil {
		os.RemoveAll(planDir)
		http.Error(w, "store plan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n > maxPlanBytes {
		os.RemoveAll(planDir)
		http.Error(w, fmt.Sprintf("plan file exceeds the %d MB limit", maxPlanBytes>>20), http.StatusRequestEntityTooLarge)
		return
	}

	instructions := planJobInstructions(planID, title, path, name, n)
	added, err := st.Ingest(agentjob.Spec{
		ID:              planID,
		Title:           "Plan: " + title,
		Instructions:    instructions,
		Driver:          "local",
		Source:          "plan",
		Class:           class,
		Priority:        priority,
		ReasoningEffort: queue.CanonicalEffort("high", "coding"),
	})
	if err != nil {
		os.RemoveAll(planDir)
		http.Error(w, "enqueue plan job: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !added {
		// Lost a race with the pre-check above.
		os.RemoveAll(planDir)
		http.Error(w, "a plan with this name was just uploaded; wait a second and retry", http.StatusConflict)
		return
	}

	job, _ := st.Get(planID)
	respondJSON(w, planUploadResponse{
		ID:        planID,
		Title:     title,
		File:      name,
		Path:      path,
		Size:      n,
		JobID:     planID,
		JobStatus: job.Status,
	})
}

// handlePlanList returns the uploaded plans, newest first (capped at
// planListMax), each paired with the status of its plan job.
func (ps *plansState) handlePlanList(s *Server, w http.ResponseWriter, r *http.Request) {
	plans, err := ps.list(s)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	respondJSON(w, plans)
}

// handlePlanListHTML renders the plan list for the dashboard's Plans section
// (hx-get /html/plans-section).
func (ps *plansState) handlePlanListHTML(s *Server, w http.ResponseWriter, r *http.Request) {
	plans, err := ps.list(s)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "plans-list", plans); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// list reads the plans directory and pairs each plan with its plan job.
// A missing directory is an empty list, not an error.
func (ps *plansState) list(s *Server) ([]planInfo, error) {
	entries, err := os.ReadDir(ps.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []planInfo{}, nil
		}
		return nil, err
	}
	st, err := s.openAgentJobStore()
	if err != nil {
		return nil, err
	}

	var plans []planInfo
	for _, e := range entries {
		if !e.IsDir() || !planIDRe.MatchString(e.Name()) {
			continue
		}
		p := planInfo{ID: e.Name(), JobStatus: "no job"}
		if j, err := st.Get(e.Name()); err == nil {
			p.Title = strings.TrimPrefix(j.Title, "Plan: ")
			p.JobStatus = j.Status
			p.UploadedAt = j.CreatedAt.UTC().Format("2006-01-02 15:04")
		}
		if files, err := os.ReadDir(filepath.Join(ps.dir, e.Name())); err == nil {
			for _, f := range files {
				if f.IsDir() {
					continue
				}
				p.File = f.Name()
				if fi, err := f.Info(); err == nil {
					p.Size = fi.Size()
				}
			}
		}
		plans = append(plans, p)
	}

	tsOf := func(id string) int64 {
		i := strings.LastIndex(id, "-")
		if i < 0 {
			return 0
		}
		ts, _ := strconv.ParseInt(id[i+1:], 10, 64)
		return ts
	}
	sort.Slice(plans, func(a, b int) bool {
		if tsOf(plans[a].ID) != tsOf(plans[b].ID) {
			return tsOf(plans[a].ID) > tsOf(plans[b].ID)
		}
		return plans[a].ID > plans[b].ID
	})
	if len(plans) > planListMax {
		plans = plans[:planListMax]
	}
	return plans, nil
}

// planJobInstructions is the body of a plan job: the recipe for the worker's
// round, which turns the uploaded plan into jobs on the queue.
func planJobInstructions(planID, title, path, name string, size int64) string {
	return fmt.Sprintf(`A plan file was uploaded from the dashboard, and this job exists so you break it into work on the queue.

Plan: %s
Plan file: %s
(%s, %d bytes)

1. Read the plan file in full. If it is not plain text (pdf, docx, zip,
   image), convert it to text first (pdftotext, unzip, …) — never guess at
   its contents.
2. Break the plan into jobs: one job per unit of work, each small enough
   for a single round, with a clear title and concrete acceptance
   criteria. Copy the relevant plan text into each job's body so the job
   stands on its own without the plan file.
3. Create the jobs in the order the work happens, starting from:

       jarvis-jobs add -id %s-1 -title "<short>" -class build -priority 30 <<'EOF'
       <job body: goal, acceptance criteria, branch, references>
       EOF

   - IDs %s-1, %s-2, … in dependency order.
   - Priority descending from 30 (30, 29, …) so the first job runs first
     within its class.
   - A job that builds on earlier output gets -depends <earlier-id>
     (e.g. -depends %s-1); independent jobs need no link.
   - Class build, unless the plan marks a piece as urgent (then fix).
   - All jobs of this plan share one branch: phase-<short-slug-of-plan>
     (the first job creates it, the rest reuse it).
4. Track the breakdown on the kanban: create an epic for the plan (POST
   /api/kanban/epics on the dashboard at http://localhost:8080) and one
   task per job (POST /api/kanban/tasks, epic_id from the epic response).
5. Notify Nathan:
       jarvis-discord notify "Plan %s: created N jobs (list the ids)"
6. This job makes no code change and opens no PR: the deliverable is the
   set of jobs on the queue. End the round with a summary of the created
   jobs (id + one line each).`,
		title, path, name, size,
		planID, planID, planID, planID,
		title)
}

// planSlug turns a file name into a short lowercase slug for the plan job
// ID: non-alphanumerics collapse to '-', at most 24 characters, "plan" when
// nothing survives.
func planSlug(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	s := planSlugRe.ReplaceAllString(strings.ToLower(base), "-")
	s = strings.Trim(s, "-")
	if len(s) > 24 {
		s = strings.Trim(s[:24], "-")
	}
	if s == "" {
		s = "plan"
	}
	return s
}

// sanitizePlanName reduces an uploaded file name to a safe single path
// element: any directory part is dropped, the character set is restricted
// to alphanumerics, '-', '_' and '.', and the result is trimmed and capped
// at 128 bytes (extension kept). Returns "" when nothing usable survives.
func sanitizePlanName(name string) string {
	base := filepath.Base(filepath.Clean("/" + name))
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, base)
	base = strings.Trim(base, " .-")
	if base == "" {
		return ""
	}
	if len(base) > 128 {
		if i := strings.LastIndexByte(base, '.'); i > 0 {
			ext := base[i:]
			base = base[:128-len(ext)] + ext
		} else {
			base = base[:128]
		}
		base = strings.Trim(base, " .-")
	}
	return base
}
