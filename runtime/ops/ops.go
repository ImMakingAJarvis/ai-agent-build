// Package ops runs tool calls outside the model (ELR-1, M3.1a, card
// ELR-24): Runner.Start returns at once and the result arrives later
// through the Runner's OnDone callback, so a job can give its slot back
// while the command runs.
//
// A Tool's Run blocks until the tool finishes; the Runner makes it
// asynchronous. Each running op has one context: a deadline (the
// call's own Timeout if set, else the class default from Runner.Timeouts)
// or, for class Long, no deadline at all — the stall rule (card M8.5)
// governs those instead. Hitting the deadline reports Status timeout,
// Runner.Cancel reports cancelled, a panic inside Run is recovered and
// reported as failed. Statuses use the agentjob op constants.
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// Class is a tool's size class, as declared in its Spec. The Runner maps
// the class to the default deadline; Long runs without one (M8.5 adds the
// stall rule).
type Class string

const (
	Quick  Class = "quick"
	Normal Class = "normal"
	Long   Class = "long"
)

// Spec describes one tool: what it is (Name, Description, Schema — the
// JSON Schema object of its arguments) and how the runtime should treat
// it.
type Spec struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Class       Class
	ExpectFast  bool
	NeedsGPU    bool
}

// Call is one tool invocation. Name selects the tool in the Registry.
type Call struct {
	Name      string
	JobID     string
	OpID      string
	Workspace string
	Args      json.RawMessage
	LogPath   string
	Timeout   time.Duration
}

// Result reports a finished op. Status uses the agentjob op constants:
// done, failed, timeout, cancelled.
type Result struct {
	Status   string
	Output   string
	ExitCode int
	Error    string
}

// Tool is one runnable tool. Run blocks until the tool finishes; the
// Runner makes it asynchronous. progress is called whenever the tool
// produces output (the Runner forwards it to its OnProgress callback).
type Tool interface {
	Spec() Spec
	Run(ctx context.Context, c Call, progress func()) Result
}

// Registry holds the tools by name.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry registers the tools, keyed by each tool's Spec().Name.
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{tools: make(map[string]Tool, len(tools))}
	for _, tool := range tools {
		r.tools[tool.Spec().Name] = tool
	}
	return r
}

// Get returns the tool registered under name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	return tool, ok
}

// Specs returns the spec of every registered tool, sorted by name.
func (r *Registry) Specs() []Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	specs := make([]Spec, 0, len(r.tools))
	for _, tool := range r.tools {
		specs = append(specs, tool.Spec())
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs
}

// Runner starts tool calls in the background and reports their results.
//
// OnProgress and OnDone are called from the op's goroutine, possibly
// concurrently for different ops; a nil callback is skipped.
type Runner struct {
	Registry   *Registry
	Timeouts   map[Class]time.Duration
	OnProgress func(jobID, opID string)
	OnDone     func(jobID, opID string, res Result)

	mu    sync.Mutex
	runs  map[string]run
	nonce int
}

// run is one live op: the cancel of its context and the nonce that
// identifies this Start, so a late finish cannot un-register a newer op
// that re-used the same OpID.
type run struct {
	cancel context.CancelFunc
	nonce  int
}

// Start runs c's tool in one goroutine and returns at once. An unknown
// tool name is an error and starts nothing.
//
// The op's deadline is c.Timeout when it is > 0, else Timeouts[class];
// for class Long there is no deadline (card M8.5 adds the stall rule).
// The result — with Status timeout when the deadline passed, cancelled
// when Cancel was called, failed with the panic text when Run panicked —
// is delivered through OnDone.
func (r *Runner) Start(c Call) error {
	tool, ok := r.Registry.Get(c.Name)
	if !ok {
		return fmt.Errorf("ops: unknown tool %q", c.Name)
	}
	spec := tool.Spec()
	ctx, cancel := r.contextFor(c, spec.Class)

	r.mu.Lock()
	if r.runs == nil { // the Runner may be built as a plain struct literal
		r.runs = make(map[string]run)
	}
	r.nonce++
	nonce := r.nonce
	r.runs[c.OpID] = run{cancel: cancel, nonce: nonce}
	r.mu.Unlock()

	go func() {
		res := runTool(ctx, tool, c, r.progress(c))
		if err := ctx.Err(); err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				res.Status = agentjob.OpTimeout
			case errors.Is(err, context.Canceled):
				res.Status = agentjob.OpCancelled
			}
		}
		if res.Status == "" {
			res.Status = agentjob.OpDone
		}
		r.finish(c.OpID, nonce, cancel)
		if r.OnDone != nil {
			r.OnDone(c.JobID, c.OpID, res)
		}
	}()
	return nil
}

// contextFor builds the op's context: c.Timeout if set, else the class
// default; Long runs without a deadline.
func (r *Runner) contextFor(c Call, class Class) (context.Context, context.CancelFunc) {
	if c.Timeout > 0 {
		return context.WithTimeout(context.Background(), c.Timeout)
	}
	if class != Long {
		if d := r.Timeouts[class]; d > 0 {
			return context.WithTimeout(context.Background(), d)
		}
	}
	return context.WithCancel(context.Background())
}

// runTool runs the tool, recovering a panic as a failed result.
func runTool(ctx context.Context, tool Tool, c Call, progress func()) (res Result) {
	defer func() {
		if p := recover(); p != nil {
			res = Result{Status: agentjob.OpFailed, Error: fmt.Sprintf("panic: %v", p)}
		}
	}()
	return tool.Run(ctx, c, progress)
}

// progress forwards the tool's output ticks to the runner's OnProgress
// callback.
func (r *Runner) progress(c Call) func() {
	return func() {
		if r.OnProgress != nil {
			r.OnProgress(c.JobID, c.OpID)
		}
	}
}

// finish un-registers a finished op (only if this Start still owns the
// OpID) and releases its context.
func (r *Runner) finish(opID string, nonce int, cancel context.CancelFunc) {
	r.mu.Lock()
	if cur, ok := r.runs[opID]; ok && cur.nonce == nonce {
		delete(r.runs, opID)
	}
	r.mu.Unlock()
	cancel()
}

// Cancel cancels the context of a running op; its result has Status
// cancelled. It reports whether an op with that ID is running.
func (r *Runner) Cancel(opID string) bool {
	r.mu.Lock()
	run, ok := r.runs[opID]
	r.mu.Unlock()
	if !ok {
		return false
	}
	run.cancel()
	return true
}

// Running returns the op ids that are running now, sorted.
func (r *Runner) Running() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.runs))
	for id := range r.runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
