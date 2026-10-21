// Package dashboard provides a web UI for monitoring the job queue and history.
package dashboard

import (
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
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
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
	ID         string `json:"id"`
	Type       string `json:"type"`
	Source     string `json:"source"`
	Model      string `json:"model"`
	UseLocal   bool   `json:"use_local"`
	Queue      string `json:"queue"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Duration   string `json:"duration,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

// QueueDepth holds the pending message count for one queue.
type QueueDepth struct {
	Name  string `json:"name"`
	Depth int    `json:"depth"`
}

// DashboardState is the full state returned by /api/state.
type DashboardState struct {
	OllamaHealthy bool          `json:"ollama_healthy"`
	Queues        []QueueDepth  `json:"queues"`
	RecentJobs    []JobRecord   `json:"recent_jobs"`
}

// Server holds the dashboard dependencies.
type Server struct {
	addr       string
	ollama     *models.Client
	historyPath string
	amqpURL    string
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

	return &Server{
		addr:          addr,
		ollama:        models.NewClient(),
		historyPath:   historyPath,
		amqpURL:       amqpURL,
	}
}

// Serve starts the HTTP server.
func (s *Server) Serve() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/jobs", s.handleJobs)
	mux.HandleFunc("GET /api/queues", s.handleQueues)

	server := &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}

	fmt.Printf("dashboard: listening on %s", s.addr)
	return server.ListenAndServe()
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data := s.gatherState()
	funcMap := template.FuncMap{
		"statusClass":    StatusClass,
		"formatDuration": FormatDuration,
	}
	tmpl, err := template.New("index.html").Funcs(funcMap).ParseFS(templatesFS, "templates/index.html")
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
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
	jobs := s.readRecentJobs(50)
	depths, _ := s.getQueueDepths()
	healthy := s.ollama.Healthcheck()
	return DashboardState{
		OllamaHealthy: healthy,
		Queues:        depths,
		RecentJobs:    jobs,
	}
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

// StatusClass returns a CSS class for a job status.
func StatusClass(status string) string {
	switch status {
	case "completed":
		return "status-completed"
	case "failed":
		return "status-failed"
	case "running":
		return "status-running"
	default:
		return ""
	}
}
