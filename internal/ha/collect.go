// Package ha gathers JARVIS headline status (queue depths, current job,
// loaded model, API spend, GPU stats) and publishes it to Home Assistant
// as MQTT auto-discovered sensors.
//
// It is the Phase 7 "headline numbers" publisher: Home Assistant gets a
// few headline sensors only — job-level views (queue, approvals, cost
// detail) live in the Phase 6 dashboard, hardware dashboards in Grafana.
package ha

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// State is one snapshot of all headline values.
type State struct {
	Queues         map[string]int // queue name -> depth
	CurrentJob     string         // title of the running/claimed agent job, "" if none
	LoadedModel    string         // model loaded in the engine, "" if unknown
	APISpendToday  float64        // $ spent (actual, else estimated) on API jobs started today (UTC)
	PowerCostToday float64        // $ electricity cost of local jobs started today (UTC)
	EnergyToday    float64        // kWh sampled from local jobs started today (UTC)
	GPUTemp        float64        // degrees C
	GPUPower       float64        // watts
	GPUOK          bool           // nvidia-smi succeeded this tick
}

// QueueDepths returns the message count of every known job queue.
// Queues that have not been declared yet are omitted.
func QueueDepths() (map[string]int, error) {
	conn, err := queue.NewConn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	ch := conn.Channel()
	if ch == nil {
		return nil, fmt.Errorf("queue: no channel")
	}
	depths := map[string]int{}
	for _, q := range queue.AllQueues {
		qch, err := ch.QueueInspect(q)
		if err != nil {
			continue // queue not declared yet (e.g. "fix")
		}
		depths[q] = qch.Messages
	}
	return depths, nil
}

// CurrentJob returns the title of the most recently claimed running or
// claimed agent job, or "" if the agent is idle.
func CurrentJob() (string, error) {
	store, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		return "", err
	}
	jobs, err := store.List()
	if err != nil {
		return "", err
	}
	title, claimedAt := "", time.Time{}
	for _, j := range jobs {
		if j.Status != agentjob.Running && j.Status != agentjob.Claimed {
			continue
		}
		t, err := time.Parse(time.RFC3339, j.ClaimedAt)
		if err != nil {
			t = time.Time{}
		}
		if t.After(claimedAt) {
			claimedAt = t
			title = j.Title
		}
	}
	return title, nil
}

// LoadedModel returns the model currently loaded in the engine at
// engineURL. It understands both runtimes that have served port 11434 in
// this build: the llama.cpp server (/v1/models lists the loaded model)
// and Ollama (/api/ps lists models resident in memory).
func LoadedModel(engineURL string) (string, error) {
	if m, err := fetchJSON(engineURL+"/v1/models", func(d []byte) (string, bool) {
		var v struct {
			Models []struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			} `json:"models"`
		}
		if json.Unmarshal(d, &v) != nil || len(v.Models) == 0 {
			return "", false
		}
		name := v.Models[0].Name
		if name == "" {
			name = v.Models[0].Model
		}
		// llama-server reports full GGUF paths; the basename is the headline.
		return filepath.Base(name), true
	}); err == nil && m != "" {
		return m, nil
	}
	if m, err := fetchJSON(engineURL+"/api/ps", func(d []byte) (string, bool) {
		var v struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if json.Unmarshal(d, &v) != nil || len(v.Models) == 0 {
			return "", false
		}
		return v.Models[0].Name, true
	}); err == nil && m != "" {
		return m, nil
	}
	return "", fmt.Errorf("no loaded model at %s", engineURL)
}

// fetchJSON GETs url and decodes the body with decode. It returns ""
// (with nil error) for 404/405 so callers can try the next endpoint.
func fetchJSON(url string, decode func([]byte) (string, bool)) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	v, ok := decode(body)
	if !ok {
		return "", nil
	}
	return v, nil
}

// TodayAPISpend sums the cost of API-dispatched jobs started on the
// current UTC day, reading only the tail of the job history file (the
// file is append-ordered, so today's records are at the end). Actual
// billed cost wins over the pre-dispatch estimate when present.
func TodayAPISpend(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}

	today := time.Now().UTC().Format("2006-01-02")
	const chunk = 512 * 1024 // today's events are always in the tail

	var total float64
	end := fi.Size()
	for end > 0 {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return 0, err
		}
		buf := make([]byte, end-start)
		if _, err := io.ReadFull(f, buf); err != nil && err != io.ErrUnexpectedEOF {
			return 0, err
		}
		// Drop the partial line at the head of the chunk (unless at file start).
		if start > 0 {
			nl := bytes.IndexByte(buf, '\n')
			if nl < 0 {
				end = start
				continue
			}
			buf = buf[nl+1:]
		}
		// Lines are oldest-first within the chunk. Stop once we hit a
		// record whose start event happened before today: everything
		// earlier in the file was written (and started) before it. A
		// completion line for an old job (started_at old, finished_at
		// today) does NOT qualify — its start event is older but it was
		// written after today's records, so keep scanning.
		stop := false
		sc := bytes.NewBuffer(buf)
		for {
			line, err := readLine(sc)
			if err == io.EOF {
				break
			}
			if len(line) == 0 {
				continue
			}
			var rec struct {
				StartedAt     string  `json:"started_at"`
				FinishedAt    string  `json:"finished_at"`
				UseLocal      bool    `json:"use_local"`
				EstimatedCost float64 `json:"estimated_cost"`
				ActualCost    float64 `json:"actual_cost"`
			}
			if json.Unmarshal(line, &rec) != nil {
				continue
			}
			if strings.HasPrefix(rec.StartedAt, today) {
				if !rec.UseLocal {
					c := rec.EstimatedCost
					if rec.ActualCost > 0 {
						c = rec.ActualCost
					}
					total += c
				}
				continue
			}
			if rec.StartedAt != "" && rec.StartedAt < today &&
				(rec.FinishedAt == "" || rec.FinishedAt < today) {
				stop = true
			}
		}
		if stop || start == 0 {
			break
		}
		end = start
	}
	return total, nil
}

// TodayPowerCost sums the sampled energy (kWh) and electricity cost ($)
// of jobs started on the current UTC day, using the same tail-scan as
// TodayAPISpend. The power fields are only populated for local agent
// jobs (see internal/power).
func TodayPowerCost(path string) (kwh, cost float64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}

	today := time.Now().UTC().Format("2006-01-02")
	const chunk = 512 * 1024 // today's events are always in the tail

	end := fi.Size()
	for end > 0 {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return 0, 0, err
		}
		buf := make([]byte, end-start)
		if _, err := io.ReadFull(f, buf); err != nil && err != io.ErrUnexpectedEOF {
			return 0, 0, err
		}
		// Drop the partial line at the head of the chunk (unless at file start).
		if start > 0 {
			nl := bytes.IndexByte(buf, '\n')
			if nl < 0 {
				end = start
				continue
			}
			buf = buf[nl+1:]
		}
		// Lines are oldest-first within the chunk; stop once we pass today.
		stop := false
		sc := bytes.NewBuffer(buf)
		for {
			line, err := readLine(sc)
			if err == io.EOF {
				break
			}
			if len(line) == 0 {
				continue
			}
			var rec struct {
				StartedAt  string  `json:"started_at"`
				FinishedAt string  `json:"finished_at"`
				Kwh        float64 `json:"kwh"`
				PowerCost  float64 `json:"power_cost"`
			}
			if json.Unmarshal(line, &rec) != nil {
				continue
			}
			if strings.HasPrefix(rec.StartedAt, today) {
				kwh += rec.Kwh
				cost += rec.PowerCost
				continue
			}
			if rec.StartedAt != "" && rec.StartedAt < today &&
				(rec.FinishedAt == "" || rec.FinishedAt < today) {
				stop = true
			}
		}
		if stop || start == 0 {
			break
		}
		end = start
	}
	return kwh, cost, nil
}

// readLine reads one newline-terminated line (without the newline).
func readLine(r *bytes.Buffer) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if err == nil {
		return line, nil
	}
	if err == io.EOF && len(line) > 0 {
		return line, nil
	}
	return line, err
}

// GPUStats reads temperature and power draw of the first GPU via
// nvidia-smi (or the command named by the HA_GPU_CMD env var, for tests).
func GPUStats() (temp, power float64, err error) {
	cmdName := "nvidia-smi"
	if c := os.Getenv("HA_GPU_CMD"); c != "" {
		cmdName = c
	}
	out, err := exec.Command(cmdName,
		"--query-gpu=temperature.gpu,power.draw",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, 0, err
	}
	// nvidia-smi CSV: "62, 370.59" — comma+space separated.
	fields := strings.Fields(strings.ReplaceAll(string(out), ",", " "))
	if len(fields) < 1 {
		return 0, 0, fmt.Errorf("gpu: empty nvidia-smi output")
	}
	t, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("gpu: parse temp %q: %w", fields[0], err)
	}
	if len(fields) < 2 {
		return t, 0, nil
	}
	p, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return t, 0, nil
	}
	return t, p, nil
}
