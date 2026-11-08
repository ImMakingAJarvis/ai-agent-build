// Package api implements the HTTP server for the OpenAPI contract in
// openapi.yaml. The spec is the source of truth for routes and shapes.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/db"
)

// Version is the API version advertised on /health.
const Version = "0.1.0"

// Server serves the Reel API over the given store.
type Server struct {
	db  *db.DB
	mux *http.ServeMux
}

// New wires the routes from openapi.yaml onto a mux.
func New(d *db.DB) *Server {
	s := &Server{db: d, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/libraries", s.handleListLibraries)
	s.mux.HandleFunc("POST /api/v1/libraries", s.handleCreateLibrary)
	s.mux.HandleFunc("GET /api/v1/libraries/{id}", s.handleGetLibrary)
	s.mux.HandleFunc("GET /api/v1/items", s.handleListItems)
	s.mux.HandleFunc("GET /api/v1/items/{id}", s.handleGetItem)
	return s
}

// Listen serves until the process is stopped.
func (s *Server) Listen(addr string) error {
	return http.ListenAndServe(addr, s.mux)
}

// Handler exposes the mux (used by tests).
func (s *Server) Handler() http.Handler { return s.mux }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, errors.New("id must be an integer")
	}
	return id, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": Version})
}

func (s *Server) handleListLibraries(w http.ResponseWriter, _ *http.Request) {
	libs, err := s.db.ListLibraries()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if libs == nil {
		libs = []db.Library{}
	}
	writeJSON(w, http.StatusOK, libs)
}

func (s *Server) handleCreateLibrary(w http.ResponseWriter, r *http.Request) {
	var in db.Library
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if in.Name == "" || in.Path == "" {
		httpError(w, http.StatusBadRequest, "name and path are required")
		return
	}
	if in.Type == "" {
		in.Type = "movies"
	}
	if in.Type != "movies" && in.Type != "shows" {
		httpError(w, http.StatusBadRequest, "type must be movies or shows")
		return
	}
	created, err := s.db.CreateLibrary(in)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleGetLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	l, err := s.db.GetLibrary(id)
	if errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusNotFound, "library not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) handleListItems(w http.ResponseWriter, r *http.Request) {
	f := db.ItemFilter{}
	if v := r.URL.Query().Get("library_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			httpError(w, http.StatusBadRequest, "library_id must be an integer")
			return
		}
		f.LibraryID = &id
	}
	if v := r.URL.Query().Get("type"); v != "" {
		if v != "movie" && v != "season" && v != "episode" {
			httpError(w, http.StatusBadRequest, "type must be movie, season or episode")
			return
		}
		f.Type = v
	}
	if v := r.URL.Query().Get("query"); v != "" {
		f.Query = v
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			httpError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		f.Limit = n
	}
	items, err := s.db.ListItems(f)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if items == nil {
		items = []db.Item{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleGetItem(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	i, err := s.db.GetItem(id)
	if errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusNotFound, "item not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, i)
}
