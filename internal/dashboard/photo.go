// Photo endpoints for the gallery page (served by the art service on :8120,
// a different origin, so they need CORS). They give the web UI the same
// photo pipeline as Discord:
//
//	POST /api/photo/assist — two-stage prompt: expand a basic request with
//	the thinking mind (like !photoassist) and return it for review.
//	POST /api/photo/jobs  — queue a photo job (prompt + optional size +
//	optional uploaded reference image), the same ingested photo-tagged job
//	a Discord !photo makes: the swap-window watcher brackets it and the
//	iGPU round generates and DMs the result.
package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/art"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photomind"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photo"
)

// photoJobPriority places gallery photos at the top of the build band, the
// same priority the Discord !photo path uses (cmd/jarvis-chat/schedule.go).
const photoJobPriority = 999

// photoAssistTimeout bounds one thinking-mind expansion.
const photoAssistTimeout = 90 * time.Second

// cors sets the headers the gallery page needs to call these endpoints from
// its own origin (the art service, :8120), and answers the preflight.
func cors(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	return true
}

// photoModel is the thinking-mind model name for photo assist: LOCAL_MODEL
// (the env the chat daemon reads), falling back to the deployed name.
func photoModel() string {
	if m := os.Getenv("LOCAL_MODEL"); m != "" {
		return m
	}
	return "jarvis-brain"
}

// handlePhotoAssist expands a basic photo request into a detailed prompt with
// the thinking mind. When the brain is down it echoes the request unchanged
// with a note, so the form still works (the job then runs on the owner's
// words, like a Discord !photo with a dead brain).
func (s *Server) handlePhotoAssist(w http.ResponseWriter, r *http.Request) {
	if !cors(w, r) || r.Method != http.MethodPost {
		if r.Method != http.MethodOptions {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	var body struct {
		Request string `json:"request"`
		Size    string `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Request) == "" {
		http.Error(w, "need a request", http.StatusBadRequest)
		return
	}
	svc := photomind.New(s.ollama, photoModel(), art.New())
	if !svc.Healthy() {
		writeJSON(w, http.StatusOK, map[string]string{
			"prompt": strings.TrimSpace(body.Request),
			"note":   "thinking mind is down — using your words as-is",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), photoAssistTimeout)
	defer cancel()
	p, err := svc.Assist(ctx, body.Request, body.Size)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"prompt": p})
}

// handlePhotoJobs queues one photo job from the gallery page. body.ref, when
// set, is a staged reference image name at the art service (from its
// /upload); it is fetched and stored as the job's reference sidecar, so the
// round generates with the reference-capable model and the result keeps the
// subject from the photo.
func (s *Server) handlePhotoJobs(w http.ResponseWriter, r *http.Request) {
	if !cors(w, r) || r.Method != http.MethodPost {
		if r.Method != http.MethodOptions {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
		Size   string `json:"size"`
		Ref    string `json:"ref"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		http.Error(w, "need a prompt", http.StatusBadRequest)
		return
	}
	dataDir := agentjob.DataDir()
	store, err := agentjob.Open(dataDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	id := newDashPhotoID()
	if err := photo.WritePrompt(dataDir, id, prompt); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if body.Size != "" {
		if err := photo.WriteSize(dataDir, id, body.Size); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	if body.Ref != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		refBytes, err := art.New().FetchRef(ctx, body.Ref)
		cancel()
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "reference: " + err.Error()})
			return
		}
		if err := photo.WriteRefBytes(dataDir, id, refBytes); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	_, err = store.Ingest(agentjob.Spec{
		ID:           id,
		Title:        "photo: " + shortTitle(prompt),
		Instructions: photo.StandardInstructions(id),
		Driver:       "igpu",
		Class:        agentjob.ClassBuild,
		Priority:     photoJobPriority,
		Source:       "dashboard",
		Tags:         []string{photo.Tag},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// newDashPhotoID builds a dashboard photo job id (a single safe path
// segment, like the chat daemon's chat-<unix>-<hex> ids).
func newDashPhotoID() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("dash-%d-%s", time.Now().Unix(), hex.EncodeToString(b))
}

// shortTitle trims a prompt for use as the job title.
func shortTitle(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// writeJSON sends one JSON response.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
