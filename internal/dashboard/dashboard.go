// Package dashboard provides a web UI for monitoring the job queue and history.
package dashboard

import (
	"bufio"
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
	"strconv"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/slots"
)

//go:embed templates/*.html
var templatesFS embed.FS

const (
	defaultAddr         = ":8080"
	defaultHistoryDir   = ""
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
	ID            string `json:"id"`
	Type          string `json:"type"`
	Priority      int    `json:"priority"`
	Queue         string `json:"queue"`
	Source        string `json:"source"`
	CreatedAt     string `json:"created_at"`
	Prompt        string `json:"prompt,omitempty"`
	ForceAPI      bool   `json:"force_api"`
	ModelOverride string `json:"model_override,omitempty"`
}

// PendingQueue holds the pending messages for one queue.
type PendingQueue struct {
	Name  string       `json:"name"`
	Depth int          `json:"depth"`
	Jobs  []PendingJob `json:"jobs"`
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
	OllamaHealthy   bool             `json:"ollama_healthy"`
	RabbitMQHealthy bool             `json:"rabbitmq_healthy"`
	Slots           []slots.SlotInfo `json:"slots"` // Engine parallel slots
	Queues          []QueueDepth     `json:"queues"`
	RecentJobs      []JobRecord      `json:"recent_jobs"`
	AgentJobs       []agentjob.Job   `json:"agent_jobs"`      // build work: ready, claimed, running, done, failed
	SlotQueues      []SlotQueue      `json:"slot_queues"`     // Currently-running jobs per slot
	TotalCost       float64          `json:"total_cost"`      // Running total of estimated costs for API jobs
	APICount        int              `json:"api_job_count"`   // Number of API-dispatched jobs
	LocalCount      int              `json:"local_job_count"` // Number of local jobs
}

// LightweightState is returned for the initial page load.
// It skips expensive JSONL reads and pending queue peeks,
// allowing the page to render immediately while sections
// load their data asynchronously via HTMX.
type LightweightState struct {
	OllamaHealthy   bool         `json:"ollama_healthy"`
	RabbitMQHealthy bool         `json:"rabbitmq_healthy"`
	Queues          []QueueDepth `json:"queues"`
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

	// Pre-compiled templates (parsed once at startup).
	tmpl *template.Template

	// State cache: avoid re-reading the JSONL file on every refresh.
	stateMu    sync.Mutex
	stateCache DashboardState
	stateTime  time.Time
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

	// Pre-compile templates once at startup.
	funcMap := template.FuncMap{
		"statusClass":    StatusClass,
		"formatDuration": FormatDuration,
		"formatCost":     FormatCost,
		"divCost":        DivCost,
		"slotRuntime":    SlotRuntime,
		"jobPath":        JobPath,
	}
	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		fmt.Fprintf(os.Stderr, "dashboard: template parse error: %v\n", err)
		os.Exit(1)
	}

	return &Server{
		addr:         addr,
		ollama:       models.NewClient(),
		engineClient: engineClient,
		historyPath:  historyPath,
		amqpURL:      amqpURL,
		authUser:     os.Getenv("DASHBOARD_USER"),
		authPass:     os.Getenv("DASHBOARD_PASS"),
		tmpl:         tmpl,
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
	mux.HandleFunc("GET /html/lightweight", s.handleLightweightHTML)
	mux.HandleFunc("GET /html/jobs-section", s.handleJobsSectionHTML)
	mux.HandleFunc("GET /html/agent-jobs-section", s.handleAgentJobsSectionHTML)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/jobs", s.handleJobs)
	mux.HandleFunc("GET /api/queues", s.handleQueues)
	mux.HandleFunc("GET /api/pending", s.handlePending)
	mux.HandleFunc("GET /api/approvals", s.handleApprovals)
	mux.HandleFunc("POST /api/approvals", s.handleApprovalAction)
	mux.HandleFunc("GET /jobs/{id}", s.handleJobLog)
	mux.HandleFunc("GET /html/jobs/{id}", s.handleJobLogHTML)
	mux.HandleFunc("GET /api/jobs/{id}/log/stream", s.handleJobLogStream)

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

// handleIndex serves the static shell. It touches no backend so the first
// paint is instant; every section loads itself via HTMX.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "index.html", nil); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// gatherLightweight collects only fast, always-fresh data (health + queue depths).
// It skips the expensive JSONL scan and pending queue peek.
func (s *Server) gatherLightweight() LightweightState {
	var st LightweightState
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		depths, err := s.getQueueDepths()
		if err != nil {
			fmt.Printf("dashboard: rabbitmq unreachable: %v\n", err)
		}
		st.Queues = depths
	}()
	go func() { defer wg.Done(); st.OllamaHealthy = s.ollama.Healthcheck() }()
	go func() { defer wg.Done(); st.RabbitMQHealthy = s.checkRabbitMQ() }()
	wg.Wait()
	return st
}

func (s *Server) handleLightweightHTML(w http.ResponseWriter, r *http.Request) {
	data := s.gatherLightweight()
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "lightweight-content", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) handleJobsSectionHTML(w http.ResponseWriter, r *http.Request) {
	cached := s.getCachedJobData()
	data := DashboardState{
		RecentJobs: cached.jobs,
		TotalCost:  cached.totalCost,
		APICount:   cached.apiCount,
		LocalCount: cached.localCount,
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "jobs-section", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) handleAgentJobsSectionHTML(w http.ResponseWriter, r *http.Request) {
	cached := s.getCachedJobData()
	data := DashboardState{
		AgentJobs: cached.agentJobs,
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "agent-jobs-section", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	data := s.gatherState()
	respondJSON(w, data)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	jobs := s.readRecentJobs(50)
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
	// Fetch lightweight, always-fresh data.
	depths, err := s.getQueueDepths()
	if err != nil {
		fmt.Printf("dashboard: rabbitmq unreachable: %v\n", err)
	}
	ollamaHealthy := s.ollama.Healthcheck()
	rabbitmqHealthy := s.checkRabbitMQ()
	engineSlots, _ := s.engineClient.GetSlots()

	// Get cached JSONL-based data (jobs, costs, agent jobs).
	cached := s.getCachedJobData()

	return DashboardState{
		OllamaHealthy:   ollamaHealthy,
		RabbitMQHealthy: rabbitmqHealthy,
		AgentJobs:       cached.agentJobs,
		Slots:           engineSlots,
		Queues:          depths,
		RecentJobs:      cached.jobs,
		TotalCost:       cached.totalCost,
		APICount:        cached.apiCount,
		LocalCount:      cached.localCount,
	}
}

// cachedJobData holds the expensive-to-compute parts of the dashboard state.
type cachedJobData struct {
	jobs       []JobRecord
	agentJobs  []agentjob.Job
	totalCost  float64
	apiCount   int
	localCount int
}

const jobDataCacheTTL = 5 * time.Second

// getCachedJobData returns job data from cache if fresh enough,
// otherwise recomputes it. This avoids re-reading the JSONL file
// and reopening the agent job store on every dashboard refresh.
// Pending queues are NOT included here — they are fetched lazily
// via the /api/pending endpoint to keep page loads fast.
func (s *Server) getCachedJobData() cachedJobData {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if time.Since(s.stateTime) < jobDataCacheTTL {
		return cachedJobData{
			jobs:       s.stateCache.RecentJobs,
			agentJobs:  s.stateCache.AgentJobs,
			totalCost:  s.stateCache.TotalCost,
			apiCount:   s.stateCache.APICount,
			localCount: s.stateCache.LocalCount,
		}
	}

	// Recompute.
	jobs := s.readRecentJobs(100)

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

	// Update cache.
	s.stateCache.RecentJobs = jobs
	s.stateCache.AgentJobs = agentJobs
	s.stateCache.TotalCost = totalCost
	s.stateCache.APICount = apiCount
	s.stateCache.LocalCount = localCount
	s.stateTime = time.Now()

	return cachedJobData{
		jobs:       jobs,
		agentJobs:  agentJobs,
		totalCost:  totalCost,
		apiCount:   apiCount,
		localCount: localCount,
	}
}

// checkRabbitMQ verifies the broker is reachable using the persistent connection.
// Falls back to a dial-only check if the persistent connection is unavailable.
func (s *Server) checkRabbitMQ() bool {
	if s.rabbitConn != nil && !s.rabbitConn.IsClosed() {
		ch := s.rabbitConn.Channel()
		if ch != nil {
			// Try a lightweight operation to confirm the connection is alive.
			_, err := ch.QueueInspect(queue.VoiceQueue)
			return err == nil
		}
		return true // connection exists but channel is nil; assume healthy
	}
	// Fallback: dial and close.
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

	// Read only the tail of the file to avoid scanning a huge JSONL.
	lines := readJSONLTail(f, limit*3) // read a few extra to account for parse failures

	// Deduplicate by ID, keeping the last (completed/failed) record for each job.
	byID := make(map[string]JobRecord)
	for _, line := range lines {
		var rec JobRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
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

// readJSONLTail reads the last ~targetLines lines from the file by seeking
// from the end. It grows the read window exponentially until it has enough
// lines or reaches the beginning of the file. This avoids scanning a
// multi-megabyte JSONL on every dashboard refresh.
func readJSONLTail(f *os.File, targetLines int) [][]byte {
	stat, err := f.Stat()
	if err != nil {
		return nil
	}
	fileSize := stat.Size()
	if fileSize == 0 {
		return nil
	}

	// Start with a window sized for ~targetLines at an estimated 256 bytes/line.
	windowSize := int64(targetLines * 256)
	if windowSize <= 0 {
		windowSize = 4096
	}

	for {
		offset := fileSize - windowSize
		if offset < 0 {
			offset = 0
		}

		f.Seek(offset, 0)
		var lines [][]byte
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 1<<20), 1<<20) // 1 MB max line
		for scanner.Scan() {
			lines = append(lines, scanner.Bytes())
		}

		if len(lines) >= targetLines || offset == 0 {
			// If we read from the middle of the file (not offset==0),
			// the first line may be partial — drop it.
			if offset > 0 && len(lines) > 0 {
				lines = lines[1:]
			}
			return lines
		}

		// Need more lines; double the window.
		windowSize *= 2
		if windowSize > fileSize {
			windowSize = fileSize
		}
	}
}

func (s *Server) readSlotQueues() []SlotQueue {
	f, err := os.Open(s.historyPath)
	if err != nil {
		return makeEmptySlotQueues()
	}
	defer f.Close()

	// Build map of running jobs by queue.
	runningByQueue := map[string]SlotQueue{}
	scanner := bufio.NewScanner(f)
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
	if s.rabbitConn == nil || s.rabbitConn.IsClosed() {
		return nil, fmt.Errorf("rabbitmq connection not available")
	}
	ch := s.rabbitConn.Channel()
	if ch == nil {
		return nil, fmt.Errorf("rabbitmq channel not available")
	}

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
	if s.rabbitConn == nil || s.rabbitConn.IsClosed() {
		return nil
	}
	ch := s.rabbitConn.Channel()
	if ch == nil {
		return nil
	}

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
	approvals := s.getDeniedJobs()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.tmpl.ExecuteTemplate(w, "approvals.html", approvals)
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

// getDeniedJobs reads the most recent denied jobs from the history file.
func (s *Server) getDeniedJobs() []ApprovalInfo {
	f, err := os.Open(s.historyPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var denied []ApprovalInfo
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var rec JobRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec.Status != "denied" {
			continue
		}
		// Find the corresponding "running" record to get the prompt.
		// Denied records don't have the prompt, but we can include the cost info.
		ai := ApprovalInfo{
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
		denied = append(denied, ai)
	}
	return denied
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
	scanner := bufio.NewScanner(f)
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
	scanner := bufio.NewScanner(f)
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

const (
	defaultLogTail = 256 << 10 // bytes sent when ?tail is not given
	maxFormatBytes = 1 << 20   // bytes parsed for the formatted view
)

// jobLogPath returns the transcript (or output) file for a job, if any.
func jobLogPath(st *agentjob.Store, id string) string {
	for _, name := range []string{"transcript.txt", "output.txt"} {
		f := filepath.Join(st.JobDir(id), name)
		if fi, err := os.Stat(f); err == nil && fi.Size() > 0 {
			return f
		}
	}
	return ""
}

// handleJobLogStream streams the raw transcript of a job without loading it
// into memory. By default only the last 256 KB are sent (?tail=N bytes;
// tail=0 sends everything), so a huge log never blocks reading.
func (s *Server) handleJobLogStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !agentjob.ValidID(id) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	st, err := agentjob.Open(filepath.Dir(s.historyPath))
	if err != nil {
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}
	filePath := jobLogPath(st, id)
	if filePath == "" {
		http.Error(w, "no log found", http.StatusNotFound)
		return
	}
	f, err := os.Open(filePath)
	if err != nil {
		http.Error(w, "could not open log", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "could not open log", http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusServiceUnavailable)
		return
	}

	tail := int64(defaultLogTail)
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			tail = n
		}
	}
	size := fi.Size()
	truncated := tail > 0 && size > tail
	if truncated {
		if _, err := f.Seek(size-tail, io.SeekStart); err != nil {
			http.Error(w, "could not seek log", http.StatusInternalServerError)
			return
		}
	}

	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Log-Size", strconv.FormatInt(size, 10))
	h.Set("X-Log-Truncated", strconv.FormatBool(truncated))

	buf := make([]byte, 32*1024)
	first := truncated // drop the partial first line after a seek
	for {
		n, err := f.Read(buf)
		chunk := buf[:n]
		if first && n > 0 {
			if i := bytes.IndexByte(chunk, '\n'); i >= 0 {
				chunk = chunk[i+1:]
				first = false
			} else {
				chunk = nil
			}
		}
		if len(chunk) > 0 {
			if _, werr := w.Write(chunk); werr != nil {
				return
			}
			flusher.Flush()
		}
		if err != nil || r.Context().Err() != nil {
			return
		}
	}
}
