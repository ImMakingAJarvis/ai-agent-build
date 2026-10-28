package dashboard

import (
	"context"
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
)

// The engine runs with --log-disable and the dashboard user cannot read the
// system journal, so when the local model dies there is nothing to look at.
// /logs shows what is reachable without privileges: the engine unit's state
// (systemctl show), a /health probe, and tails of the jarvis state-dir logs.

const maxLogTail = 1 << 20

// logFiles are the tail-able logs, keyed by the ?file= value.
var logFiles = map[string]string{
	"agent":     "agent.log",
	"dashboard": "dashboard.log",
	"worker":    "worker-new.log",
	"ha":        "ha-publisher.log",
}

func stateLogDir() string {
	if d := os.Getenv("JARVIS_STATE_DIR"); d != "" {
		return d
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/agent"
	}
	return filepath.Join(home, ".local", "state", "jarvis")
}

// EngineStatus is the engine unit state shown on the logs page.
type EngineStatus struct {
	Unit        string `json:"unit"`
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state"`
	Result      string `json:"result"`
	ExitStatus  string `json:"exit_status"`
	Since       string `json:"since"`
	Restarts    string `json:"restarts"`
	Healthy     bool   `json:"healthy"`
	Health      string `json:"health"`
}

func (s *Server) engineStatus(ctx context.Context) EngineStatus {
	const unit = "jarvis-engine"
	st := EngineStatus{Unit: unit}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "systemctl", "show", unit,
		"-p", "ActiveState,SubState,Result,ExecMainStatus,StateChangeTimestamp,NRestarts").Output()
	if err != nil {
		st.ActiveState = "unknown"
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "ActiveState":
			st.ActiveState = v
		case "SubState":
			st.SubState = v
		case "Result":
			st.Result = v
		case "ExecMainStatus":
			st.ExitStatus = v
		case "StateChangeTimestamp":
			st.Since = v
		case "NRestarts":
			st.Restarts = v
		}
	}
	// Probe the engine itself; a stopped unit and a hung server differ.
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, strings.TrimRight(s.engineClient.BaseURL, "/")+"/health", nil)
	resp, herr := s.engineClient.HTTP.Do(req)
	if herr != nil {
		st.Health = "unreachable: " + herr.Error()
		return st
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	st.Healthy = resp.StatusCode == http.StatusOK
	st.Health = fmt.Sprintf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	return st
}

// tailFile returns the last n bytes of path, starting on a line boundary.
func tailFile(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	trunc := fi.Size() > n
	if trunc {
		if _, err := f.Seek(fi.Size()-n, io.SeekStart); err != nil {
			return "", err
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	s := string(b)
	if trunc {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return s, nil
}

// handleEngineLogs serves GET /api/engine/logs: engine status plus a log
// tail. ?file=agent|dashboard|worker|ha (default agent), ?tail=bytes.
func (s *Server) handleEngineLogs(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("file")
	if name == "" {
		name = "agent"
	}
	rel, ok := logFiles[name]
	if !ok {
		http.Error(w, "unknown log file", http.StatusBadRequest)
		return
	}
	tail := int64(64 << 10)
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			tail = min(n, maxLogTail)
		}
	}
	resp := struct {
		Engine EngineStatus `json:"engine"`
		File   string       `json:"file"`
		Log    string       `json:"log"`
		Error  string       `json:"error,omitempty"`
	}{Engine: s.engineStatus(r.Context()), File: name}
	log, err := tailFile(filepath.Join(stateLogDir(), rel), tail)
	if err != nil {
		resp.Error = err.Error()
	}
	resp.Log = log
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(resp)
}

const logsPage = `<!doctype html><meta charset=utf-8><meta name=viewport content="width=device-width,initial-scale=1">
<title>Jarvis logs</title>
<style>body{font:14px system-ui;margin:1rem;background:#111;color:#ddd}a{color:#8cf}
#st{padding:.5rem;border-radius:4px;margin:.5rem 0}.ok{background:#143}.bad{background:#611}
pre{background:#000;padding:.5rem;overflow:auto;max-height:75vh;white-space:pre-wrap;word-break:break-word}</style>
<p><a href="/">&larr; dashboard</a></p><h1>Logs</h1>
<div id=st>loading…</div>
<p>file: <select id=f><option>agent<option>dashboard<option>worker<option>ha</select>
<label><input type=checkbox id=a checked> auto-refresh</label></p>
<pre id=log></pre>
<script>
async function load(){
 try{
  const r=await fetch('/api/engine/logs?file='+f.value);const j=await r.json(),e=j.engine;
  st.className=e.healthy?'ok':'bad';
  st.textContent='engine '+e.unit+': '+e.active_state+'/'+e.sub_state+' since '+e.since+
   ' (result '+e.result+', exit '+e.exit_status+', restarts '+e.restarts+') — health: '+e.health;
  const stick=log.scrollTop+log.clientHeight>=log.scrollHeight-20;
  log.textContent=j.error?('error: '+j.error):j.log;
  if(stick)log.scrollTop=log.scrollHeight;
 }catch(x){st.className='bad';st.textContent='dashboard fetch failed: '+x}
}
f.onchange=load;load();setInterval(()=>{if(a.checked)load()},5000);
</script>`

func (s *Server) handleLogsPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, logsPage)
}
