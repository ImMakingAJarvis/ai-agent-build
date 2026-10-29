package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/redis/go-redis/v9"
)

// seedMaxLines bounds the history a new connection receives from the
// Redis stream; older output is still in the transcript file.
const seedMaxLines = 5000

// handleJobLive streams a job's log live as SSE:
//
//	meta   job state (id, title, status, live)
//	seed   events parsed from everything known so far
//	step   one live event (or delta) as it is published
//	done   the job stopped; the connection closes
//
// Live events come from the job's Redis stream (written by the runner
// as the agent works). Without Redis — or for jobs that predate it —
// the endpoint tails the transcript file instead, so the page always
// shows progress, just with a ~2 s delay in the fallback.
func (s *Server) handleJobLive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !agentjob.ValidID(id) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	st, err := agentjob.Open(filepath.Dir(s.historyPath))
	if err != nil {
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}
	j, err := st.Get(id)
	if err != nil {
		http.Error(w, "no such job", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	live := j.Status == agentjob.Running || j.Status == agentjob.Claimed
	writeSSE(w, flusher, "meta", map[string]any{
		"id": id, "title": j.Title, "status": j.Status,
		"driver": j.RanWith, "model": j.Model, "live": live,
	})

	if s.redis != nil {
		if events, last, ok := s.seedFromStream(r.Context(), id); ok {
			writeSSE(w, flusher, "seed", map[string]any{"events": events})
			s.streamTail(w, flusher, r, id, last, st)
			return
		}
	}
	// No stream (job predates Redis, or Redis is down): parse the
	// transcript file for the seed and keep tailing it.
	events, seedPos, err := s.seedFromStore(st, id)
	if err == nil {
		writeSSE(w, flusher, "seed", map[string]any{"events": events})
	}
	s.fileTail(w, flusher, r, id, st, seedPos)
}

// seedFromStream returns events parsed from the job's Redis stream and
// the ID of its last entry (to resume XREAD from). ok is false when the
// stream does not exist or is empty.
func (s *Server) seedFromStream(ctx context.Context, id string) ([]LiveEvent, string, bool) {
	key := agentjob.StreamKey(id)
	entries, err := s.redis.XRevRangeN(ctx, key, "+", "-", seedMaxLines).Result()
	if err != nil || len(entries) == 0 {
		return nil, "", false
	}
	// XRevRangeN returns newest first; parse oldest to newest.
	p := &LiveParser{}
	var events []LiveEvent
	for i := len(entries) - 1; i >= 0; i-- {
		if raw, ok := entries[i].Values["data"]; ok {
			if s, ok := raw.(string); ok {
				events = append(events, p.Feed(s)...)
			}
		}
	}
	events = append(events, p.Flush()...)
	return events, entries[0].ID, true
}

// streamTail follows the job's Redis stream until the job stops or the
// client goes away.
func (s *Server) streamTail(w http.ResponseWriter, f http.Flusher, r *http.Request, id, lastID string, st *agentjob.Store) {
	ctx := r.Context()
	p := &LiveParser{}
	for {
		if ctx.Err() != nil {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		res, err := s.redis.XRead(ctx, &redis.XReadArgs{
			Streams: []string{agentjob.StreamKey(id), lastID},
			Block:   3 * time.Second,
		}).Result()
		cancel()
		if err == nil && len(res) > 0 && len(res[0].Messages) > 0 {
			for _, m := range res[0].Messages {
				lastID = m.ID
				if raw, ok := m.Values["data"]; ok {
					if s, ok := raw.(string); ok {
						for _, ev := range p.Feed(s) {
							writeSSE(w, f, "step", ev)
						}
					}
				}
			}
			for _, ev := range p.Flush() {
				writeSSE(w, f, "step", ev)
			}
		}
		// The block timeout doubles as a status poll and a heartbeat.
		if j, err := st.Get(id); err == nil && j.Status != agentjob.Running && j.Status != agentjob.Claimed {
			for _, ev := range p.Flush() {
				writeSSE(w, f, "step", ev)
			}
			writeSSE(w, f, "done", map[string]any{"status": j.Status, "error": j.Error})
			return
		}
		fmt.Fprintf(w, ": ping %d\n\n", time.Now().Unix())
		f.Flush()
	}
}

// fileTail is the no-Redis fallback: it re-reads the transcript file
// every couple of seconds (from the byte offset already sent) and
// appends the new complete lines, until the job stops or the client goes
// away. startPos skips what the seed already sent.
func (s *Server) fileTail(w http.ResponseWriter, f http.Flusher, r *http.Request, id string, st *agentjob.Store, startPos int64) {
	ctx := r.Context()
	path := jobLogPath(st, id)
	pos := startPos
	p := &LiveParser{}
	for {
		if ctx.Err() != nil {
			return
		}
		if tf, err := os.Open(path); err == nil {
			if fi, err := tf.Stat(); err == nil {
				size := fi.Size()
				// A smaller file than the seed position means the job
				// re-ran and truncated the log: start over.
				if size < pos {
					pos = 0
					p = &LiveParser{}
				}
				if size > pos {
					if b, err := io.ReadAll(io.NewSectionReader(tf, pos, size-pos)); err == nil {
						// Feed only complete lines (up to the last
						// newline); the trailing partial line is
						// picked up next tick.
						if nl := bytes.LastIndexByte(b, '\n'); nl >= 0 {
							for _, line := range strings.Split(string(b[:nl+1]), "\n") {
								for _, ev := range p.Feed(line) {
									writeSSE(w, f, "step", ev)
								}
							}
							pos += int64(nl) + 1
						}
					}
				}
			}
			tf.Close()
		}
		if j, err := st.Get(id); err == nil && j.Status != agentjob.Running && j.Status != agentjob.Claimed {
			for _, ev := range p.Flush() {
				writeSSE(w, f, "step", ev)
			}
			writeSSE(w, f, "done", map[string]any{"status": j.Status, "error": j.Error})
			return
		}
		fmt.Fprintf(w, ": ping %d\n\n", time.Now().Unix())
		f.Flush()
		time.Sleep(2 * time.Second)
	}
}

// seedFromStore parses the job's transcript file (bounded tail, like the
// formatted view) into events and returns the byte offset where the
// seeded region ends, so fileTail can resume right after it.
func (s *Server) seedFromStore(st *agentjob.Store, id string) ([]LiveEvent, int64, error) {
	path := jobLogPath(st, id)
	if path == "" {
		return nil, 0, os.ErrNotExist
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	pos := int64(0)
	if fi.Size() > maxFormatBytes {
		pos = fi.Size() - maxFormatBytes
	}
	b, err := io.ReadAll(io.NewSectionReader(f, pos, fi.Size()-pos))
	if err != nil {
		return nil, 0, err
	}
	if pos > 0 {
		// Match readJobTail: drop the partial leading line.
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
			pos += int64(i) + 1
		}
	}
	p := &LiveParser{}
	var events []LiveEvent
	for _, line := range strings.Split(string(b), "\n") {
		events = append(events, p.Feed(line)...)
	}
	events = append(events, p.Flush()...)
	return events, pos, nil
}

// writeSSE sends one named SSE event with a JSON payload and flushes.
func writeSSE(w http.ResponseWriter, f http.Flusher, event string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	f.Flush()
}
