// Package api is loopd's HTTP layer (M4.1). Card ELR-93 (M4.1a) added the
// token loader and the bearer-token guard; card ELR-94 (M4.1b) adds the
// Server: the read side of the API — a job's spec, state and ops, its
// conversation log, and the runtime's state. Every string in a served
// message or log passes the Redactor before it is written to the response
// (edge case 2026-11-24): no endpoint returns raw text.
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/ops"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/redact"
)

// Deps is what the Server needs to serve the read side (M4.1b) and the
// write side (M4.1c): the job store (specs, states, ops, messages), the
// op runner (which ops are live right now), the Redactor, the bearer
// token for the Guard, the version string the daemon was built with (a
// later card serves it), and Submit — the POST /v1/tasks creation path.
// Submitter is what the default Submit hands the new task id to (the
// loop's Submit, wired by cmd/loopd).
type Deps struct {
	Store    *agentjob.Store
	Runner   *ops.Runner
	Redactor *redact.Redactor
	Token    string
	Version  string
	// Submit creates a task from POST /v1/tasks (M4.1c). nil means the
	// default: Ingest the task as a ready loop job, then Submitter(id).
	// Card m4-2d wraps it with the submit checks.
	Submit func(TaskRequest) (string, error)
	// Submitter hands a new task id to the runtime (loop.Submit); nil
	// leaves the job ready in the store.
	Submitter func(jobID string) error
}

// Server is loopd's HTTP server: the read and write sides of the API
// behind the bearer-token Guard.
type Server struct {
	deps Deps
}

// New builds a Server from its dependencies, installing the default
// Submit (write.go) when none is given.
func New(d Deps) *Server {
	if d.Submit == nil {
		d.Submit = d.defaultSubmit
	}
	return &Server{deps: d}
}

// Version returns the version string the server was built with.
func (s *Server) Version() string { return s.deps.Version }

// Draining reports whether the server is draining: it accepts no new work
// and lets running work finish. Until card m4-1e adds the drain state and
// POST /v1/drain, it always reports false.
func (s *Server) Draining() bool { return false }

// Handler returns the server's root handler: a ServeMux with the read
// routes (M4.1b) and the write routes (M4.1c), wrapped in the
// bearer-token Guard (ELR-93) — /healthz passes without a token, every
// /v1 route requires one.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/state", s.handleState)
	mux.HandleFunc("GET /v1/tasks/{id}", s.handleGetTask)
	mux.HandleFunc("GET /v1/tasks/{id}/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/tasks", s.handleCreateTask)
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", s.handleCancel)
	mux.HandleFunc("POST /v1/tasks/{id}/messages", s.handleAddMessage)
	mux.HandleFunc("POST /v1/tasks/{id}/approvals", s.handleApprovals)
	return Guard(s.deps.Token, mux)
}

// handleHealthz answers the probe: 200 and the text ok (specs/loopd.md).
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("ok")); err != nil {
		// The client went away; there is nothing left to do.
	}
}

// handleGetTask serves GET /v1/tasks/{id}: the job's spec and state (Job
// embeds both, so one object carries both) plus its ops. An unknown id is
// 404 {"error":"not found"}.
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := s.deps.Store.Get(id)
	if err != nil {
		notFound(w)
		return
	}
	jobOps, err := s.deps.Store.Ops(id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "ops: %v", err)
		return
	}
	if jobOps == nil {
		jobOps = []agentjob.LoopOp{}
	}
	s.writeJSON(w, map[string]any{"job": job, "ops": jobOps})
}

// handleMessages serves GET /v1/tasks/{id}/messages: the job's
// conversation log (messages.jsonl) as a JSON array. A job with no log
// yet is an empty array; an unknown job is 404.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.deps.Store.Get(id); err != nil {
		notFound(w)
		return
	}
	msgs, err := s.deps.Store.Messages(id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "messages: %v", err)
		return
	}
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m)
	}
	s.writeJSON(w, out)
}

// handleState serves GET /v1/state: the draining flag (false until card
// m4-1e), the ops running now, and the job counts by status.
func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	jobs, err := s.deps.Store.List()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list jobs: %v", err)
		return
	}
	counts := make(map[string]int)
	for _, j := range jobs {
		counts[j.Status]++
	}
	running := []map[string]string{}
	if s.deps.Runner != nil {
		// The Runner knows which ops are live but not which job they
		// belong to; the store is the join. An op id is not job-scoped
		// (accept-1 exists in every job with acceptance commands), so
		// the match needs the store's running status as well as the id.
		live := make(map[string]bool)
		for _, opID := range s.deps.Runner.Running() {
			live[opID] = true
		}
		for _, j := range jobs {
			jobOps, err := s.deps.Store.Ops(j.ID)
			if err != nil {
				httpError(w, http.StatusInternalServerError, "ops %s: %v", j.ID, err)
				return
			}
			for _, op := range jobOps {
				if live[op.ID] && op.Status == agentjob.OpRunning {
					running = append(running, map[string]string{"job": j.ID, "op": op.ID})
				}
			}
		}
		sort.Slice(running, func(a, b int) bool {
			if running[a]["job"] != running[b]["job"] {
				return running[a]["job"] < running[b]["job"]
			}
			return running[a]["op"] < running[b]["op"]
		})
	}
	s.writeJSON(w, map[string]any{
		"draining":    s.Draining(),
		"running_ops": running,
		"jobs":        counts,
	})
}

// writeJSON redacts v (every string through the Redactor) and writes it
// as a JSON response.
func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	b, err := s.redacted(v)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "encode: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(b); err != nil {
		// The client went away; there is nothing left to do.
	}
}

// redacted marshals v to JSON, applies the Redactor to every string in
// the value, and re-marshals it. The round trip through a generic value
// is what lets one walker cover structs, maps and nested content alike.
func (s *Server) redacted(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return nil, err
	}
	return json.Marshal(s.redactValue(generic))
}

// redactValue applies the Redactor to every string inside a decoded JSON
// value (the shapes encoding/json unmarshals into: map, slice, string,
// float64, bool, nil) and returns the value. A nil Redactor is a no-op
// (Apply on a nil *Redactor returns its input).
func (s *Server) redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = s.redactValue(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = s.redactValue(val)
		}
		return t
	case string:
		return s.deps.Redactor.Apply(t)
	default:
		return v
	}
}

// notFound answers 404 {"error":"not found"}.
func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	if _, err := io.WriteString(w, `{"error":"not found"}`); err != nil {
		// The client went away; there is nothing left to do.
	}
}

// httpError answers a JSON error body with code.
func httpError(w http.ResponseWriter, code int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, err := fmt.Fprintf(w, `{"error":%q}`, fmt.Sprintf(format, args...)); err != nil {
		// The client went away; there is nothing left to do.
	}
}
