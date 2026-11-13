package superagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/catalog"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/switchboard"
)

const (
	defaultShellTimeout = 120 * time.Second
	maxShellTimeout     = 600 * time.Second
	maxReadLines        = 400
	defaultReadLines    = 200
	maxOutput           = 4000 // observation size cap, protects the context
	maxFetchBody        = 3000
)

// ToolSpec is one tool's name and one-line description, for the prompt.
type ToolSpec struct {
	Name        string
	Description string
}

// Specs lists the tools the executor can call, in prompt order.
func Specs() []ToolSpec {
	return []ToolSpec{
		{"shell", `{"cmd": "...", "timeout": 120} — run a command in the work dir (seconds, max 600)`},
		{"read_file", `{"path": "...", "offset": 1, "limit": 200} — read a file, 1-based line offset`},
		{"write_file", `{"path": "...", "content": "..."} — create/overwrite a file in the work dir`},
		{"list_dir", `{"path": "."} — list a directory`},
		{"memory_search", `{"query": "...", "k": 5} — search the memory graph of past work`},
		{"rabbitmq", `{"op":"publish|consume|peek|depth|caps|submit|listen|yield","queue":"flow.x","title":"...","instructions":"...","n":1,"capability":"image.describe","corr":"c1","input":{"path":"..."},"input-ref":"...","out-dir":"...","action":"wake|forward|notify","target":"...","wait-on":"c1","note":"..."} — events + capabilities (docs/event-driven.md): publish/consume/peek/depth on queues; caps lists the capability catalog; submit posts an envelope-v2 request to cap.<capability> (Result lands on the job's flow queue); listen attaches a switchboard listener; yield (job-backed runs) parks the run until its wait-on results arrive`},
		{"web_fetch", `{"url": "..."} — fetch a URL and return the first part of the body`},
		{"step_done", `{"note": "..."} — this step is achieved; note how it was verified`},
		{"replan", `{"reason": "..."} — the plan no longer fits; the planner runs again`},
		{"finish", `{"summary": "...", "ok": true} — the goal is done (ok) or failed (!ok)`},
	}
}

// Tools executes the file- and shell-tools. Control tools (step_done,
// replan, finish) are handled by the agent loop.
type Tools struct {
	Guard     *Guard
	MemoryURL string // jarvis-memory base URL; "" disables memory_search
	HTTP      *http.Client
	// Yielded is set when a rabbitmq yield op succeeded: the run ends as
	// "yielded" (exit 0; the agent job stays Waiting for the switchboard
	// to wake it — the same contract as a brain round that yielded).
	Yielded bool
}

// NewTools builds the default toolset for workdir.
func NewTools(workdir string) *Tools {
	return &Tools{
		Guard:     &Guard{WorkDir: workdir},
		MemoryURL: "http://localhost:8101",
		HTTP:      &http.Client{Timeout: 60 * time.Second},
	}
}

// Run executes one tool action and returns the observation the model sees.
func (t *Tools) Run(ctx context.Context, a Action) (string, error) {
	switch a.Tool {
	case "shell":
		return t.runShell(ctx, a)
	case "read_file":
		return t.runRead(a)
	case "write_file":
		return t.runWrite(a)
	case "list_dir":
		return t.runList(a)
	case "memory_search":
		return t.runMemory(ctx, a)
	case "rabbitmq":
		return t.runMQ(ctx, a)
	case "web_fetch":
		return t.runFetch(ctx, a)
	default:
		return "", fmt.Errorf("unknown tool %q", a.Tool)
	}
}

func (t *Tools) runShell(ctx context.Context, a Action) (string, error) {
	cmd := argStr(a.Args, "cmd", "")
	if strings.TrimSpace(cmd) == "" {
		return "", fmt.Errorf("shell needs a cmd")
	}
	if reason, blocked := t.Guard.CheckCommand(cmd); blocked {
		return "BLOCKED: " + reason, nil
	}
	timeout := defaultShellTimeout
	if ms := time.Duration(argInt(a.Args, "timeout", int(defaultShellTimeout.Seconds()))) * time.Second; ms > 0 {
		timeout = ms
		if timeout > maxShellTimeout {
			timeout = maxShellTimeout
		}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(cctx, "sh", "-c", cmd)
	c.Dir = t.Guard.WorkDir
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	obs := truncate(out.String(), maxOutput)
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		return fmt.Sprintf("TIMED OUT after %s\n%s", timeout, obs), nil
	case err != nil:
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Sprintf("exit %d\n%s", ee.ExitCode(), obs), nil
		}
		return "", err
	default:
		return obs, nil
	}
}

func (t *Tools) runRead(a Action) (string, error) {
	path := argStr(a.Args, "path", "")
	abs, reason, blocked := t.Guard.CheckPath(path)
	if blocked {
		return "BLOCKED: " + reason, nil
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(b), "\n")
	off := argInt(a.Args, "offset", 1)
	if off < 1 {
		off = 1
	}
	limit := argInt(a.Args, "limit", defaultReadLines)
	if limit < 1 || limit > maxReadLines {
		limit = defaultReadLines
	}
	var sb strings.Builder
	end := off + limit - 1
	for i := off - 1; i < len(lines) && i < end; i++ {
		fmt.Fprintf(&sb, "%d\t%s\n", i+1, lines[i])
	}
	if end > len(lines) {
		fmt.Fprintf(&sb, "(file has %d lines)", len(lines))
	}
	return truncate(sb.String(), maxOutput), nil
}

func (t *Tools) runWrite(a Action) (string, error) {
	path := argStr(a.Args, "path", "")
	abs, reason, blocked := t.Guard.CheckPath(path)
	if blocked {
		return "BLOCKED: " + reason, nil
	}
	content := argStr(a.Args, "content", "")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), filepath.Base(abs)), nil
}

func (t *Tools) runList(a Action) (string, error) {
	path := argStr(a.Args, "path", ".")
	abs, reason, blocked := t.Guard.CheckPath(path)
	if blocked {
		return "BLOCKED: " + reason, nil
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name()+"/")
		} else {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(empty directory)", nil
	}
	return truncate(strings.Join(names, "\n"), maxOutput), nil
}

func (t *Tools) runMemory(ctx context.Context, a Action) (string, error) {
	if t.MemoryURL == "" {
		return "memory search is disabled", nil
	}
	query := argStr(a.Args, "query", "")
	if query == "" {
		return "", fmt.Errorf("memory_search needs a query")
	}
	k := argInt(a.Args, "k", 5)
	if k < 1 || k > 10 {
		k = 5
	}
	body, _ := json.Marshal(map[string]any{"query": query, "k": k})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.MemoryURL+"/search", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return "memory service unreachable: " + err.Error(), nil
	}
	defer resp.Body.Close()
	var out struct {
		Results []struct {
			Title string  `json:"title"`
			Text  string  `json:"text"`
			Score float64 `json:"score"`
			Path  string  `json:"path"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode memory search: %w", err)
	}
	if len(out.Results) == 0 {
		return "(no results)", nil
	}
	var sb strings.Builder
	for i, r := range out.Results {
		fmt.Fprintf(&sb, "%d. %s (score %.2f, %s)\n   %s\n", i+1, r.Title, r.Score, r.Path, truncate(strings.TrimSpace(r.Text), 240))
	}
	return truncate(sb.String(), maxOutput), nil
}

// saRandSuffix is a short random suffix for published message ids, so two
// publishes in the same second never collide (the worker uses the id as
// the agent-job id when it ingests kind=agent).
func saRandSuffix() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b)
}

// runMQ is the event-driven handoff tool: publish a job to a queue for
// another model to pick up, consume the result one left there, or — the
// capability ops (EVB-32) — list the catalog (caps), post an envelope-v2
// request to a capability worker (submit), attach a switchboard listener
// (listen), or park the run until its results arrive (yield). It is the
// in-process form of the jarvis-mq CLI: the same queue libraries, the same
// envelope, the same catalog file — reuse, not a fork.
func (t *Tools) runMQ(ctx context.Context, a Action) (string, error) {
	op := argStr(a.Args, "op", "")
	switch op {
	case "caps":
		return t.runMQCaps()
	case "submit":
		return t.runMQSubmit(ctx, a)
	case "listen":
		return t.runMQListen(ctx, a)
	case "yield":
		return t.runMQYield(a)
	case "publish", "consume", "peek", "depth":
		// the queue ops, below
	default:
		return "", fmt.Errorf("rabbitmq op must be publish, consume, peek, depth, caps, submit, listen or yield (got %q)", op)
	}

	q := argStr(a.Args, "queue", "")
	if q == "" {
		return "", fmt.Errorf("rabbitmq %s needs a queue", op)
	}
	conn, err := queue.NewConn()
	if err != nil {
		return "rabbitmq unreachable: " + err.Error(), nil
	}
	defer conn.Close()

	switch op {
	case "publish":
		payload := map[string]string{}
		if c := argStr(a.Args, "context", ""); c != "" {
			payload["context"] = c
		}
		if p := argStr(a.Args, "prompt", ""); p != "" {
			payload["prompt"] = p
		}
		m := queue.Message{
			ID:           "sa-" + strconv.FormatInt(time.Now().Unix(), 10) + "-" + saRandSuffix(),
			Type:         "event",
			CreatedAt:    time.Now().UTC(),
			Source:       "superagent",
			Payload:      payload,
			Kind:         argStr(a.Args, "kind", ""),
			Title:        argStr(a.Args, "title", ""),
			Instructions: argStr(a.Args, "instructions", ""),
		}
		if queue.IsFlowQueue(q) {
			if err := conn.PublishFlow(ctx, q, m); err != nil {
				return "", err
			}
		} else {
			if err := conn.Publish(m, q+".default"); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("published %s → %s", m.ID, q), nil
	case "consume", "peek":
		n := argInt(a.Args, "n", 1)
		var (
			msgs []queue.Message
			err  error
		)
		if op == "consume" {
			msgs, err = conn.PopFlow(q, n)
		} else {
			msgs, err = conn.PeekFlow(q, n)
		}
		if err != nil {
			return "", err
		}
		if len(msgs) == 0 {
			return q + ": empty", nil
		}
		var sb strings.Builder
		for _, m := range msgs {
			fmt.Fprintf(&sb, "--- %s (kind=%s)\n", m.ID, m.Kind)
			if m.Title != "" {
				sb.WriteString("title: " + m.Title + "\n")
			}
			if m.Instructions != "" {
				sb.WriteString("instructions: " + m.Instructions + "\n")
			}
			for _, k := range sortedArgKeys(m.Payload) {
				fmt.Fprintf(&sb, "payload[%s]: %s\n", k, m.Payload[k])
			}
		}
		return truncate(sb.String(), maxOutput), nil
	case "depth":
		d, err := conn.QueueDepth(q)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s: %d", q, d), nil
	default:
		return "", fmt.Errorf("rabbitmq op must be publish, consume, peek, depth, caps, submit, listen or yield (got %q)", op)
	}
}

// runMQCaps renders the capability catalog the way `jarvis-mq caps` does —
// one line per capability: where it runs, what it costs, the queue depth
// right now, how long a request usually takes. The catalog is the repo's
// config/capabilities.yaml ($JARVIS_CAPABILITIES to override; the machine
// path is the fallback when the work dir is not a checkout) — the same file
// the round preamble and resourced read.
func (t *Tools) runMQCaps() (string, error) {
	path := os.Getenv("JARVIS_CAPABILITIES")
	if path == "" {
		if _, err := os.Stat("config/capabilities.yaml"); err == nil {
			path = "config/capabilities.yaml"
		} else {
			path = "/home/agent/ai-agent-build/config/capabilities.yaml"
		}
	}
	f, err := catalog.Load(path)
	if err != nil {
		return "caps: " + err.Error(), nil
	}
	conn, cerr := queue.NewConn()
	if cerr != nil {
		// The catalog is still useful when the broker is down: depths as "-".
		conn = nil
	}
	var sb strings.Builder
	for _, name := range f.SortedNames() {
		c := f.ByName(name)
		depths := "-"
		if conn != nil {
			d, derr := conn.QueueDepth(queue.CapQueueName(name))
			if derr == nil {
				depths = strconv.Itoa(d)
			} else {
				var aerr *amqp.Error
				if !errors.As(derr, &aerr) || aerr.Code != amqp.NotFound {
					depths = "?" // a real failure: do not pretend the queue is empty
				}
			}
		}
		rt, rerr := c.TypicalRuntime()
		rts := "unknown"
		if rerr == nil {
			rts = rt.String()
		}
		fmt.Fprintf(&sb, "%s\twhere=%s\tcost=%s\tdepth=%s\ttypical~%s\n",
			name, strings.Join(c.Locations(), ","), c.Cost(), depths, rts)
	}
	if conn != nil {
		conn.Close()
	}
	return truncate(sb.String(), maxOutput), nil
}

// runMQSubmit posts an envelope-v2 capability request to cap.<capability>
// (plan §3.3) — the in-process form of `jarvis-mq submit`. The Result lands
// on the reply queue; for a job-backed run that defaults to the job's own
// flow queue (flow.$JARVIS_JOB_ID), which is what the switchboard's wake
// listener (op=listen) and the yield (op=yield) key on.
func (t *Tools) runMQSubmit(ctx context.Context, a Action) (string, error) {
	capability := strings.TrimSpace(argStr(a.Args, "capability", ""))
	if capability == "" {
		return "", fmt.Errorf("rabbitmq submit needs a capability (e.g. image.describe; op=caps lists them)")
	}
	if !queue.ValidCapabilityName(capability) {
		return "", fmt.Errorf("capability %q is not valid: a dotted path (image.generate) or a single word (echo), lowercase alphanumeric segments", capability)
	}
	jobID := os.Getenv("JARVIS_JOB_ID")
	replyTo := strings.TrimSpace(argStr(a.Args, "reply-to", ""))
	if replyTo == "" {
		if jobID != "" {
			replyTo = "flow." + jobID
		} else {
			replyTo = "flow.sa-" + strconv.FormatInt(time.Now().Unix(), 10) + "-" + saRandSuffix()
		}
	}
	corr := strings.TrimSpace(argStr(a.Args, "corr", ""))
	if corr == "" {
		corr = "sa-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	input := map[string]any{}
	if raw, ok := argRaw(a.Args, "input"); ok {
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &input); err != nil {
			return "", fmt.Errorf("rabbitmq submit input must be an object of strings: %v", err)
		}
	}
	var refs []string
	if raw, ok := argRaw(a.Args, "input-ref"); ok {
		switch v := raw.(type) {
		case string:
			if v != "" {
				refs = append(refs, v)
			}
		case []any:
			for _, e := range v {
				if s, ok := e.(string); ok && s != "" {
					refs = append(refs, s)
				}
			}
		}
	}
	var deadline *time.Time
	if ds := argStr(a.Args, "deadline", ""); ds != "" {
		tm, err := queue.ParseDeadline(ds)
		if err != nil {
			return "", fmt.Errorf("rabbitmq submit deadline: %v", err)
		}
		deadline = &tm
	}
	m := queue.Message{
		ID:            "sa-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Type:          "capability",
		CreatedAt:     time.Now().UTC(),
		Source:        "superagent",
		Payload:       map[string]string{},
		SchemaVersion: 2,
		Capability:    capability,
		ReplyTo:       replyTo,
		CorrelationID: corr,
		WakeJob:       argStr(a.Args, "wake-job", jobID),
		Input:         input,
		InputRefs:     refs,
		OutDir:        argStr(a.Args, "out-dir", ""),
		Location:      argStr(a.Args, "location", ""),
		Deadline:      deadline,
	}
	conn, err := queue.NewConn()
	if err != nil {
		return "rabbitmq unreachable: " + err.Error(), nil
	}
	defer conn.Close()
	qname := queue.CapQueueName(capability)
	if err := conn.EnsureCapQueue(qname); err != nil {
		return "", err
	}
	if queue.IsFlowQueue(replyTo) {
		if err := conn.EnsureFlowQueue(replyTo); err != nil {
			return "", err
		}
	}
	if err := conn.Publish(m, qname+".default"); err != nil {
		return "", err
	}
	return fmt.Sprintf("submitted %s (corr %s) → %s; the Result lands on %s", capability, corr, qname, replyTo), nil
}

// runMQListen attaches a durable switchboard listener (plan §3.2) — the
// in-process form of `jarvis-mq listen`. When a message the listener claims
// lands on the queue, the switchboard wakes the job (action=wake),
// republishes it (forward), or notifies Discord (notify).
func (t *Tools) runMQListen(ctx context.Context, a Action) (string, error) {
	q := strings.TrimSpace(argStr(a.Args, "queue", ""))
	if q == "" {
		return "", fmt.Errorf("rabbitmq listen needs a queue")
	}
	action := argStr(a.Args, "action", switchboard.ActionWake)
	corr := strings.TrimSpace(argStr(a.Args, "corr", ""))
	target := strings.TrimSpace(argStr(a.Args, "target", ""))
	l := switchboard.Listener{
		ID:       "lsn-" + q + (func() string { if corr == "" { return "" }; return "-" + corr })() + "-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + saRandSuffix(),
		Queue:    q,
		Action:   action,
		Timeout:  time.Duration(argInt(a.Args, "timeout", 0)) * time.Second,
		TimeoutAction: argStr(a.Args, "timeout-action", ""),
		CorrFilter: corr,
	}
	switch action {
	case switchboard.ActionWake:
		jobID := argStr(a.Args, "job", os.Getenv("JARVIS_JOB_ID"))
		if jobID == "" {
			return "", fmt.Errorf("rabbitmq listen action=wake needs a job (pass 'job' or run inside an agent job with JARVIS_JOB_ID set)")
		}
		l.JobID = jobID
	case switchboard.ActionForward:
		if target == "" {
			return "", fmt.Errorf("rabbitmq listen action=forward needs a target queue")
		}
		l.TargetQueue = target
	case switchboard.ActionNotify:
		l.Text = target
		if l.Text == "" {
			l.Text = "result landed on " + q
		}
	default:
		return "", fmt.Errorf("rabbitmq listen action must be wake, forward or notify (got %q)", action)
	}
	if ds := argStr(a.Args, "timeout", ""); ds != "" && argInt(a.Args, "timeout", 0) == 0 {
		if d, err := time.ParseDuration(ds); err == nil {
			l.Timeout = d
		}
	}
	conn, err := queue.NewConn()
	if err != nil {
		return "rabbitmq unreachable: " + err.Error(), nil
	}
	defer conn.Close()
	switch {
	case queue.IsFlowQueue(q):
		err = conn.EnsureFlowQueue(q)
	case queue.IsCapQueue(q):
		err = conn.EnsureCapQueue(q)
	default:
		return "", fmt.Errorf("rabbitmq listen: only flow.* and cap.* queues can be listened (got %q)", q)
	}
	if err != nil {
		return "", err
	}
	dir := os.Getenv("LISTENERS_DIR")
	if dir == "" {
		dir = filepath.Join(agentjob.DataDir(), "listeners")
	}
	st, err := switchboard.OpenStore(dir)
	if err != nil {
		return "", err
	}
	if err := st.Create(l); err != nil {
		return "", err
	}
	return fmt.Sprintf("listening on %s: %s listener %s (corr %s)", q, action, l.ID, orAny(corr)), nil
}

// orAny renders a correlation filter for humans.
func orAny(corr string) string {
	if corr == "" {
		return "any"
	}
	return corr
}

// runMQYield parks the run on capability results (plan §3.2 step 9): it
// records the wait on the agent job and flags the toolset so the agent loop
// ends the run as "yielded". The switchboard's wake listener then resumes
// the job when every wait-on correlation has arrived — the woken run
// consumes the results from its flow queue. Only a job-backed run can
// yield; a standalone run should consume the result instead.
func (t *Tools) runMQYield(a Action) (string, error) {
	jobID := os.Getenv("JARVIS_JOB_ID")
	if jobID == "" {
		return "", fmt.Errorf("rabbitmq yield needs an agent job (no JARVIS_JOB_ID in env) — submit + listen on a flow queue and consume the result instead")
	}
	waitOn := strings.TrimSpace(argStr(a.Args, "wait-on", ""))
	if waitOn == "" {
		return "", fmt.Errorf("rabbitmq yield needs wait-on: the comma-separated correlation ids to wait for")
	}
	var corrs []string
	for _, c := range strings.Split(waitOn, ",") {
		if c = strings.TrimSpace(c); c != "" {
			corrs = append(corrs, c)
		}
	}
	if len(corrs) == 0 {
		return "", fmt.Errorf("rabbitmq yield: wait-on has no ids")
	}
	note := strings.TrimSpace(argStr(a.Args, "note", ""))
	if note == "" {
		return "", fmt.Errorf("rabbitmq yield needs a note: the continuation (what the woken round should do)")
	}
	deadline := 2 * time.Hour // the round's default wait, like a brain yield
	if raw, ok := argRaw(a.Args, "deadline"); ok {
		switch v := raw.(type) {
		case string:
			if d, err := time.ParseDuration(v); err == nil {
				deadline = d
			} else if tm, err := queue.ParseDeadline(v); err == nil {
				if u := time.Until(tm); u > 0 {
					deadline = u
				}
			}
		case float64:
			if v > 0 {
				deadline = time.Duration(v) * time.Second
			}
		}
	}
	st, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		return "", fmt.Errorf("rabbitmq yield: open job store: %v", err)
	}
	waitUntil := time.Now().Add(deadline)
	if err := st.Yield(jobID, corrs, waitUntil, note); err != nil {
		return "", fmt.Errorf("rabbitmq yield: %v", err)
	}
	t.Yielded = true
	return fmt.Sprintf("yielded: job %s waits on %s until %s (note: %s)", jobID, strings.Join(corrs, ","), waitUntil.Format(time.RFC3339), note), nil
}

func sortedArgKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (t *Tools) runFetch(ctx context.Context, a Action) (string, error) {
	url := argStr(a.Args, "url", "")
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("web_fetch needs an http(s) url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return "fetch failed: " + err.Error(), nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBody+1024))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, truncate(string(b), maxFetchBody)), nil
}

// truncate caps s at n chars, keeping the head and tail (errors and
// results usually sit at the end of command output).
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		if s == "" {
			return "(no output)"
		}
		return s
	}
	head, tail := n*5/8, n/8
	return s[:head] + fmt.Sprintf("\n… [%d chars truncated] …\n", len(s)-head-tail) + s[len(s)-tail:]
}
