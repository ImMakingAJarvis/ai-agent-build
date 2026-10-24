// Package dashboard provides a web UI for monitoring the job queue and history.
package dashboard

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/slots"
)

//go:embed templates/*.html
var templatesFS embed.FS

const (
	defaultAddr       = ":8080"
	defaultHistoryDir = ""
	defaultPollInterval = 5 // seconds
)

// JobRecord mirrors queue.JobRecord for API responses.
type JobRecord struct {
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	Source        string  `json:"source"`
	Model         string  `json:"model"`
	UseLocal      bool    `json:"use_local"`
	Queue         string  `json:"queue"`
	StartedAt     string  `json:"started_at"`
	FinishedAt    string  `json:"finished_at,omitempty"`
	Duration      string  `json:"duration,omitempty"`
	Status        string  `json:"status"`
	Error         string  `json:"error,omitempty"`
	EstimatedCost float64 `json:"estimated_cost,omitempty"`
	ActualCost    float64 `json:"actual_cost,omitempty"`
	InputTokens   int     `json:"input_tokens,omitempty"`
	OutputTokens  int     `json:"output_tokens,omitempty"`
	Kind          string  `json:"kind,omitempty"`
	Title         string  `json:"title,omitempty"`
	Driver        string  `json:"driver,omitempty"`
	PRURL         string  `json:"pr_url,omitempty"`
	Summary       string  `json:"summary,omitempty"`
}

// QueueDepth holds the pending message count for one queue.
type QueueDepth struct {
	Name  string `json:"name"`
	Depth int    `json:"depth"`
}

// PendingJob represents a single message sitting in a queue.
type PendingJob struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Priority    int    `json:"priority"`
	Queue       string `json:"queue"`
	Source      string `json:"source"`
	CreatedAt   string `json:"created_at"`
	Prompt      string `json:"prompt,omitempty"`
	ForceAPI    bool   `json:"force_api"`
	ModelOverride string `json:"model_override,omitempty"`
}

// PendingQueue holds the pending messages for one queue.
type PendingQueue struct {
	Name    string       `json:"name"`
	Depth   int          `json:"depth"`
	Jobs    []PendingJob `json:"jobs"`
}

// ApprovalInfo describes a job denied by the cost approval gate.
type ApprovalInfo struct {
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	Queue         string  `json:"queue"`
	Model         string  `json:"model"`
	EstimatedCost float64 `json:"estimated_cost"`
	InputTokens   int     `json:"input_tokens"`
	OutputTokens  int     `json:"output_tokens"`
	Source        string  `json:"source"`
	StartedAt     string  `json:"started_at"`
	Prompt        string  `json:"prompt,omitempty"`
}

// SlotQueue represents the state of one queue's processing slot.
type SlotQueue struct {
	Name     string `json:"name"`
	Occupied bool   `json:"occupied"`
	JobID    string `json:"job_id,omitempty"`
	Model    string `json:"model,omitempty"`
	Runtime  string `json:"runtime,omitempty"`
}

// DashboardState is the full state returned by /api/state.
type DashboardState struct {
	OllamaHealthy  bool               `json:"ollama_healthy"`
	RabbitMQHealthy bool              `json:"rabbitmq_healthy"`
	Slots          []slots.SlotInfo   `json:"slots"`           // Engine parallel slots
	Queues         []QueueDepth       `json:"queues"`
	PendingQueues  []PendingQueue     `json:"pending_queues"`
	RecentJobs     []JobRecord        `json:"recent_jobs"`
	AgentJobs      []agentjob.Job     `json:"agent_jobs"`      // build work: ready, claimed, running, done, failed
	SlotQueues     []SlotQueue        `json:"slot_queues"`     // Currently-running jobs per slot
	TotalCost      float64            `json:"total_cost"`      // Running total of estimated costs for API jobs
	APICount       int                `json:"api_job_count"`   // Number of API-dispatched jobs
	LocalCount     int                `json:"local_job_count"` // Number of local jobs
	AgentJobTotal  int                `json:"agent_job_total,omitempty"`
	RecentJobTotal int                `json:"recent_job_total,omitempty"`
}

// ApprovalPage is one window of the denied-job list.
type ApprovalPage struct {
	Items []ApprovalInfo
	ListPage
}

// AgentJobsPage is one window of the agent job store.
type AgentJobsPage struct {
	Items []agentjob.Job
	ListPage
}

// RecentJobsPage is cost cards plus one window of jobs.jsonl.
type RecentJobsPage struct {
	Items     []JobRecord
	TotalCost float64
	APICount  int
	LocalCount int
	ListPage
}

// Server holds the dashboard dependencies.
type Server struct {
	addr         string
	ollama       *models.Client
	engineClient *slots.Client
	historyPath  string
	amqpURL      string
	rabbitConn   *queue.Conn // persistent connection for peeking at pending jobs
	authUser     string
	authPass     string

	listMu      sync.Mutex
	deniedCache []ApprovalInfo
	deniedAt    time.Time
	recentCache []JobRecord
	recentAt    time.Time
}

// New creates a dashboard server.
func New() *Server {
	addr := os.Getenv("DASHBOARD_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	historyDir := os.Getenv("JARVIS_DATA_DIR")
	if historyDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/home/agent"
		}
		historyDir = filepath.Join(home, ".local", "share", "jarvis")
	}
	historyPath := filepath.Join(historyDir, "jobs.jsonl")

	amqpURL := os.Getenv("RABBITMQ_URL")
	if amqpURL == "" {
		amqpURL = "amqp://jarvis:jarvis-local@localhost:5672"
	}

	engineClient := slots.NewClient()

	return &Server{
		addr:         addr,
		ollama:       models.NewClient(),
		engineClient: engineClient,
		historyPath:  historyPath,
		amqpURL:      amqpURL,
		authUser:     os.Getenv("DASHBOARD_USER"),
		authPass:     os.Getenv("DASHBOARD_PASS"),
	}
}

// StartRabbitMQ creates a persistent RabbitMQ connection for peeking at pending jobs.
// Call this after New() if you want pending job details in the dashboard.
func (s *Server) StartRabbitMQ() error {
	conn, err := queue.NewConn()
	if err != nil {
		return fmt.Errorf("dashboard rabbitmq: %w", err)
	}
	s.rabbitConn = conn
	return nil
}

// CloseRabbitMQ shuts down the persistent RabbitMQ connection.
func (s *Server) CloseRabbitMQ() {
	if s.rabbitConn != nil {
		s.rabbitConn.Close()
	}
}

// Serve starts the HTTP server.
func (s *Server) Serve() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /html/dashboard", s.handleDashboardHTML)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/jobs", s.handleJobs)
	mux.HandleFunc("GET /api/queues", s.handleQueues)
	mux.HandleFunc("GET /api/pending", s.handlePending)
	mux.HandleFunc("GET /api/approvals", s.handleApprovals)
	mux.HandleFunc("POST /api/approvals", s.handleApprovalAction)
	mux.HandleFunc("GET /jobs/{id}", s.handleJobLog)
	mux.HandleFunc("GET /html/jobs/{id}", s.handleJobLogHTML)
	mux.HandleFunc("GET /html/lightweight", s.handleLightweight)
	mux.HandleFunc("GET /html/jobs-section", s.handleJobsSection)
	mux.HandleFunc("GET /html/recent-jobs", s.handleRecentJobsMore)
	mux.HandleFunc("GET /html/agent-jobs-section", s.handleAgentJobsSection)
	mux.HandleFunc("GET /html/approvals", s.handleApprovals)

	var handler http.Handler = mux
	if s.authUser != "" && s.authPass != "" {
		handler = s.basicAuth(handler)
		fmt.Printf("dashboard: basic auth enabled (user: %s)", s.authUser)
	}

	server := &http.Server{
		Addr:    s.addr,
		Handler: handler,
	}

	fmt.Printf("dashboard: listening on %s", s.addr)
	return server.ListenAndServe()
}

// basicAuth returns a middleware that requires HTTP Basic Authentication.
// When enabled, every request must include valid credentials.
func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != s.authUser || pass != s.authPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Jarvis Dashboard"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Shell only — lists load in fragments so first paint stays small.
	s.renderTmpl(w, "index.html", DashboardState{})
}

func (s *Server) renderTmpl(w http.ResponseWriter, name string, data any) {
	tmpl, err := template.New("root").Funcs(jobLogFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) handleDashboardHTML(w http.ResponseWriter, r *http.Request) {
	s.renderTmpl(w, "dashboard-content", s.gatherState())
}

func (s *Server) handleLightweight(w http.ResponseWriter, r *http.Request) {
	s.renderTmpl(w, "lightweight-content", DashboardState{
		OllamaHealthy:   s.ollama.Healthcheck(),
		RabbitMQHealthy: s.checkRabbitMQ(),
		Queues:          mustQueues(s),
	})
}

func mustQueues(s *Server) []QueueDepth {
	d, _ := s.getQueueDepths()
	return d
}

func (s *Server) handleJobsSection(w http.ResponseWriter, r *http.Request) {
	s.renderTmpl(w, "jobs-section", s.recentJobsPage(0))
}

func (s *Server) handleRecentJobsMore(w http.ResponseWriter, r *http.Request) {
	s.renderTmpl(w, "recent-jobs-more", s.recentJobsPage(parseOffset(r)))
}

func (s *Server) handleAgentJobsSection(w http.ResponseWriter, r *http.Request) {
	off := parseOffset(r)
	page := s.agentJobsPage(off)
	name := "agent-jobs-section"
	if off > 0 {
		name = "agent-jobs-more"
	}
	s.renderTmpl(w, name, page)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	data := s.gatherState()
	respondJSON(w, data)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobs := s.readRecentJobs(listPageSize)
	respondJSON(w, jobs)
}

func (s *Server) handleQueues(w http.ResponseWriter, r *http.Request) {
	depths, err := s.getQueueDepths()
	if err != nil {
		respondJSON(w, []QueueDepth{})
		return
	}
	respondJSON(w, depths)
}

func (s *Server) gatherState() DashboardState {
	jobs := s.uniqueRecentJobs()
	depths, err := s.getQueueDepths()
	if err != nil {
		fmt.Printf("dashboard: rabbitmq unreachable: %v\n", err)
	}
	pending := s.getPendingQueues()
	ollamaHealthy := s.ollama.Healthcheck()
	rabbitmqHealthy := s.checkRabbitMQ()
	engineSlots, _ := s.engineClient.GetSlots()

	// Compute running cost totals from completed/denied API jobs.
	var totalCost float64
	var apiCount, localCount int
	for _, rec := range jobs {
		if rec.UseLocal {
			localCount++
		} else {
			apiCount++
			if rec.EstimatedCost > 0 {
				totalCost += rec.EstimatedCost
			} else if rec.ActualCost > 0 {
				totalCost += rec.ActualCost
			}
		}
	}

	var agentJobs []agentjob.Job
	if st, err := agentjob.Open(filepath.Dir(s.historyPath)); err == nil {
		agentJobs, _ = st.List()
	}
	agentJobs = orderAgentJobs(agentJobs)
	agentTotal := len(agentJobs)
	agentPage, _ := window(agentJobs, 0, listPageSize)

	recentTotal := len(jobs)
	recentPage, _ := window(jobs, 0, listPageSize)

	return DashboardState{
		OllamaHealthy:   ollamaHealthy,
		RabbitMQHealthy: rabbitmqHealthy,
		AgentJobs:       agentPage,
		AgentJobTotal:   agentTotal,
		Slots:           engineSlots,
		Queues:          depths,
		PendingQueues:   pending,
		RecentJobs:      recentPage,
		RecentJobTotal:  recentTotal,
		TotalCost:       totalCost,
		APICount:        apiCount,
		LocalCount:      localCount,
	}
}

// checkRabbitMQ dials RabbitMQ and closes the connection to verify
// the broker is reachable. Returns true if the connection succeeds.
func (s *Server) checkRabbitMQ() bool {
	conn, err := amqp.Dial(s.amqpURL)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (s *Server) readRecentJobs(limit int) []JobRecord {
	f, err := os.Open(s.historyPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	// Read all lines, then take the last N.
	var all []JobRecord
	scanner := newLineReader(f)
	for scanner.Scan() {
		var rec JobRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		all = append(all, rec)
	}

	// Deduplicate by ID, keeping the last (completed/failed) record for each job.
	byID := make(map[string]JobRecord)
	for _, rec := range all {
		byID[rec.ID] = rec
	}

	unique := make([]JobRecord, 0, len(byID))
	for _, rec := range byID {
		unique = append(unique, rec)
	}

	// Sort by started_at descending (most recent first).
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].StartedAt > unique[j].StartedAt
	})

	if len(unique) > limit {
		unique = unique[:limit]
	}
	return unique
}

func (s *Server) readSlotQueues() []SlotQueue {
	f, err := os.Open(s.historyPath)
	if err != nil {
		return makeEmptySlotQueues()
	}
	defer f.Close()

	// Build map of running jobs by queue.
	runningByQueue := map[string]SlotQueue{}
	scanner := newLineReader(f)
	for scanner.Scan() {
		var rec JobRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Status != "running" {
			continue
		}
		start, err := time.Parse(time.RFC3339, rec.StartedAt)
		if err != nil {
			continue
		}
		elapsed := time.Since(start).Round(time.Second)
		runningByQueue[rec.Queue] = SlotQueue{
			Name:     rec.Queue,
			Occupied: true,
			JobID:    rec.ID,
			Model:    rec.Model,
			Runtime:  elapsed.String(),
		}
	}

	return buildSlotQueues(runningByQueue)
}

func makeEmptySlotQueues() []SlotQueue {
	return buildSlotQueues(nil)
}

func buildSlotQueues(running map[string]SlotQueue) []SlotQueue {
	queues := []string{queue.VoiceQueue, queue.CodingQueue, queue.BackgroundQueue}
	slots := make([]SlotQueue, 0, len(queues))
	for _, q := range queues {
		if rq, ok := running[q]; ok {
			slots = append(slots, rq)
		} else {
			slots = append(slots, SlotQueue{Name: q, Occupied: false})
		}
	}
	return slots
}

func (s *Server) getQueueDepths() ([]QueueDepth, error) {
	conn, err := amqp.Dial(s.amqpURL)
	if err != nil {
		return nil, fmt.Errorf("dial rabbitmq: %w", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("open channel: %w", err)
	}
	defer ch.Close()

	var depths []QueueDepth
	for _, q := range queue.AllQueues {
		getCh, err := ch.QueueInspect(q)
		if err != nil {
			continue
		}
		depths = append(depths, QueueDepth{
			Name:  q,
			Depth: getCh.Messages,
		})
	}
	return depths, nil
}

// getPendingQueues peeks at pending messages in each queue.
// Uses a temporary consumer to read up to 50 messages per queue without acking.
func (s *Server) getPendingQueues() []PendingQueue {
	conn, err := amqp.Dial(s.amqpURL)
	if err != nil {
		return nil
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return nil
	}
	defer ch.Close()

	var pending []PendingQueue
	for _, qname := range queue.AllQueues {
		info, err := ch.QueueInspect(qname)
		if err != nil {
			continue
		}

		pq := PendingQueue{Name: qname, Depth: info.Messages}

		// Peek: consume with noAck to read messages without removing them.
		// Limit to 50 messages per queue to avoid blocking.
		if info.Messages > 0 {
			dels, err := ch.Consume(qname, "", false, true, false, false, nil)
			if err == nil {
				for i := 0; i < info.Messages && i < 50; i++ {
					if del, ok := <-dels; ok {
						msg, err := queue.DecodeMessage(del.Body)
						if err != nil {
							continue
						}
						pj := PendingJob{
							ID:            msg.ID,
							Type:          msg.Type,
							Priority:      msg.Priority,
							Queue:         qname,
							Source:        msg.Source,
							CreatedAt:     msg.CreatedAt.Format(time.RFC3339),
							ForceAPI:      msg.ForceAPI,
							ModelOverride: msg.ModelOverride,
						}
						if p, ok := msg.Payload["prompt"]; ok {
							pj.Prompt = p
						}
						pq.Jobs = append(pq.Jobs, pj)
					}
				}
				// Cancel the consumer to stop receiving.
				ch.Cancel("", false)
			}
		}
		pending = append(pending, pq)
	}
	return pending
}

func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	pending := s.getPendingQueues()
	respondJSON(w, pending)
}

func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	off := parseOffset(r)
	items, info := window(s.listDeniedJobs(), off, listPageSize)
	page := ApprovalPage{Items: items, ListPage: info}
	name := "approvals"
	if off > 0 {
		name = "approvals-more"
	}
	s.renderTmpl(w, name, page)
}

func (s *Server) handleApprovalAction(w http.ResponseWriter, r *http.Request) {
	// Parse form or JSON body for action and id.
	id := r.URL.Query().Get("id")
	action := r.URL.Query().Get("action") // "approve" or "reject"
	if id == "" || (action != "approve" && action != "reject") {
		http.Error(w, "require ?id=...&action=approve|reject", http.StatusBadRequest)
		return
	}

	if action == "reject" {
		// Reject: purge the message with this ID from its queue.
		// We search all queues for a message with this ID and nack it.
		s.rejectJob(id)
		fmt.Fprint(w, "rejected")
		return
	}

	// Approve: re-enqueue the job with auto_approve set.
	// The worker will pick it up and the approval gate will pass.
	s.approveJob(id)
	fmt.Fprint(w, "approved")
}

// listDeniedJobs returns unique denied jobs, newest first. The jsonl file is
// not modified — older rows stay on disk and load via ?offset=.
func (s *Server) listDeniedJobs() []ApprovalInfo {
	s.listMu.Lock()
	defer s.listMu.Unlock()
	if s.deniedCache != nil && time.Since(s.deniedAt) < 10*time.Second {
		return s.deniedCache
	}
	f, err := os.Open(s.historyPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	byID := make(map[string]ApprovalInfo)
	scanner := newLineReader(f)
	for scanner.Scan() {
		var rec JobRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Status != "denied" {
			continue
		}
		byID[rec.ID] = ApprovalInfo{
			ID:            rec.ID,
			Type:          rec.Type,
			Queue:         rec.Queue,
			Model:         rec.Model,
			EstimatedCost: rec.EstimatedCost,
			InputTokens:   rec.InputTokens,
			OutputTokens:  rec.OutputTokens,
			Source:        rec.Source,
			StartedAt:     rec.StartedAt,
		}
	}
	denied := make([]ApprovalInfo, 0, len(byID))
	for _, ai := range byID {
		denied = append(denied, ai)
	}
	sort.Slice(denied, func(i, j int) bool {
		if denied[i].StartedAt != denied[j].StartedAt {
			return denied[i].StartedAt > denied[j].StartedAt
		}
		return denied[i].ID < denied[j].ID
	})
	s.deniedCache, s.deniedAt = denied, time.Now()
	return denied
}

func (s *Server) uniqueRecentJobs() []JobRecord {
	s.listMu.Lock()
	if s.recentCache != nil && time.Since(s.recentAt) < 10*time.Second {
		out := s.recentCache
		s.listMu.Unlock()
		return out
	}
	s.listMu.Unlock()
	all := s.readRecentJobs(1 << 20)
	s.listMu.Lock()
	s.recentCache, s.recentAt = all, time.Now()
	s.listMu.Unlock()
	return all
}

func (s *Server) recentJobsPage(offset int) RecentJobsPage {
	all := s.uniqueRecentJobs()
	var totalCost float64
	var apiCount, localCount int
	for _, rec := range all {
		if rec.UseLocal {
			localCount++
		} else {
			apiCount++
			if rec.EstimatedCost > 0 {
				totalCost += rec.EstimatedCost
			} else if rec.ActualCost > 0 {
				totalCost += rec.ActualCost
			}
		}
	}
	items, info := window(all, offset, listPageSize)
	return RecentJobsPage{Items: items, TotalCost: totalCost, APICount: apiCount, LocalCount: localCount, ListPage: info}
}

func (s *Server) agentJobsPage(offset int) AgentJobsPage {
	var jobs []agentjob.Job
	if st, err := agentjob.Open(filepath.Dir(s.historyPath)); err == nil {
		jobs, _ = st.List()
	}
	items, info := window(orderAgentJobs(jobs), offset, listPageSize)
	return AgentJobsPage{Items: items, ListPage: info}
}

// approveJob re-enqueues a denied job with auto_approve set, so the worker
// will pass the approval gate. This is a best-effort operation.
func (s *Server) approveJob(id string) {
	// Find the denied record to reconstruct the message.
	f, err := os.Open(s.historyPath)
	if err != nil {
		return
	}
	defer f.Close()

	var target JobRecord
	scanner := newLineReader(f)
	for scanner.Scan() {
		var rec JobRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec.ID == id && rec.Status == "denied" {
			target = rec
			break
		}
	}

	if target.ID == "" {
		return
	}

	// Reconstruct a message and publish it.
	msg := queue.Message{
		ID:        id,
		Type:      target.Type,
		Priority:  5,
		Payload:   map[string]string{"auto_approve": "true"},
		CreatedAt: time.Now(),
		Source:    "dashboard",
	}
	if target.Queue != "" {
		msg.Type = target.Queue // use the queue name as the type for routing
	}
	routingKey := target.Queue + ".default"
	if s.rabbitConn != nil {
		s.rabbitConn.Publish(msg, routingKey)
	}
}

// rejectJob attempts to remove a denied job's message from the queue.
// This is a best-effort operation that purges matching messages.
func (s *Server) rejectJob(id string) {
	// Find which queue the denied job is in.
	f, err := os.Open(s.historyPath)
	if err != nil {
		return
	}
	defer f.Close()

	var queueName string
	scanner := newLineReader(f)
	for scanner.Scan() {
		var rec JobRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec.ID == id && rec.Status == "denied" {
			queueName = rec.Queue
			break
		}
	}

	if queueName == "" {
		return
	}

	// Connect and try to consume + nack the specific message.
	conn, err := amqp.Dial(s.amqpURL)
	if err != nil {
		return
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return
	}
	defer ch.Close()

	dels, err := ch.Consume(queueName, "", false, false, false, false, nil)
	if err != nil {
		return
	}
	defer ch.Cancel("", false)

	for del := range dels {
		msg, err := queue.DecodeMessage(del.Body)
		if err != nil {
			del.Nack(false, false)
			continue
		}
		if msg.ID == id {
			del.Nack(false, false) // reject, do not requeue
			return
		}
		del.Ack(false) // put back other messages
	}
}

func respondJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// lineReader reads lines from an io.Reader, handling potentially large files.
type lineReader struct {
	r       io.Reader
	buf     []byte
	line    []byte
	err     error
	maxLine int
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{r: r, maxLine: 1 << 20} // 1 MB max line
}

func (lr *lineReader) Scan() bool {
	var line []byte
	var err error
	for {
		line, err = lr.readLine()
		if err != nil || len(line) == 0 {
			lr.err = err
			return false
		}
		// Skip empty lines.
		if len(strings.TrimSpace(string(line))) > 0 {
			lr.line = line
			return true
		}
	}
}

func (lr *lineReader) Bytes() []byte { return lr.line }

func (lr *lineReader) Err() error { return lr.err }

func (lr *lineReader) readLine() ([]byte, error) {
	lr.buf = lr.buf[:0]
	for {
		b := make([]byte, 1)
		n, err := lr.r.Read(b)
		if n == 0 {
			if err != nil {
				return lr.buf, err
			}
			continue
		}
		if lr.buf == nil && err != nil {
			return nil, err
		}
		if b[0] == '\n' {
			return lr.buf, err
		}
		lr.buf = append(lr.buf, b[0])
		if len(lr.buf) > lr.maxLine {
			return lr.buf, nil
		}
	}
}

// FormatDuration formats a duration string for display.
func FormatDuration(d string) string {
	if d == "" {
		return "-"
	}
	t, err := time.ParseDuration(d)
	if err != nil {
		return d
	}
	if t < time.Second {
		return t.Round(time.Millisecond).String()
	}
	return t.Round(time.Second).String()
}

// FormatCost formats a dollar amount for display.
func FormatCost(c float64) string {
	if c == 0 {
		return "-"
	}
	if c < 1 {
		return fmt.Sprintf("$%.4f", c)
	}
	return fmt.Sprintf("$%.2f", c)
}

// StatusClass returns a CSS class for a job status.
func StatusClass(status string) string {
	switch status {
	case "completed":
		return "status-completed"
	case "failed":
		return "status-failed"
	case "running":
		return "status-running"
	case "denied":
		return "status-denied"
	default:
		return ""
	}
}

// SlotRuntime formats a runtime string for the active slots display.
// Returns a human-readable string like "2m 30s".
func SlotRuntime(d string) string {
	t, err := time.ParseDuration(d)
	if err != nil {
		return d
	}
	if t < time.Second {
		return t.Round(time.Millisecond).String()
	}
	m := int(t.Minutes())
	s := int(t.Seconds()) % 60
	if m > 0 && s > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%ds", s)
}

// DivCost divides a total cost by a count, returning the average.
// Used by the HTML template to compute average cost per API job.
func DivCost(total float64, count int) float64 {
	if count == 0 {
		return 0
	}
	return total / float64(count)
}
