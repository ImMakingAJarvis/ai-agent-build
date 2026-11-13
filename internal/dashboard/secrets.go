// Dashboard handlers for the secrets page: add, remove, and list-by-name.
// A value crosses the wire exactly once — in the POST body that stores it —
// and no endpoint, error message or log line ever returns it. The local
// jarvis-secrets CLI is the reader (scripts/jarvis-secrets).
package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/secrets"
)

type secretsState struct {
	store *secrets.Store
}

func initSecrets(historyDir string) *secretsState {
	store, err := secrets.Open(historyDir)
	if err != nil {
		panic("secrets store: " + err.Error())
	}
	return &secretsState{store: store}
}

// handleSecretsPage renders the standalone secrets page (GET /secrets).
func (ss *secretsState) handleSecretsPage(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		if err := s.tmpl.ExecuteTemplate(&buf, "secrets.html", nil); err != nil {
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		buf.WriteTo(w)
	}
}

// handleSecretsList serves GET /api/secrets: names, sizes and update times
// only.
func (ss *secretsState) handleSecretsList(w http.ResponseWriter, r *http.Request) {
	list, err := ss.store.List()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []secrets.Info{}
	}
	respondJSON(w, list)
}

type secretsAddRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// handleSecretsAdd serves POST /api/secrets. The value is written to disk
// and dropped; errors report the name or the reason, never the value.
func (ss *secretsState) handleSecretsAdd(w http.ResponseWriter, r *http.Request) {
	var req secretsAddRequest
	// 2 MiB read cap: one value over (MaxValueLen) plus JSON overhead.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if err := ss.store.Put(req.Name, req.Value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	respondJSON(w, map[string]bool{"ok": true})
}

// handleSecretsRemove serves DELETE /api/secrets/{name}.
func (ss *secretsState) handleSecretsRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := secrets.ValidateName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := ss.store.Delete(name); err != nil {
		if _, err2 := ss.store.Get(name); err2 != nil {
			// Name validated above and Get confirms the secret is gone:
			// report 404 for a missing secret, 500 for a real IO failure.
			http.Error(w, "no such secret", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	respondJSON(w, map[string]bool{"ok": true})
}
