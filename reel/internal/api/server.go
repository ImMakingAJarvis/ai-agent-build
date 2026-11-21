// Package api implements the HTTP server for the OpenAPI contract in
// openapi.yaml. The spec is the source of truth for routes and shapes.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/db"
)

// Version is the API version advertised on /health.
const Version = "0.1.0"

// ScanRunner enqueues the job that runs a created scan. cmd/reeld wires
// it to the RabbitMQ publisher; a server built without one (contract
// tests, no-broker dev) leaves scans in the queued state.
type ScanRunner interface {
	EnqueueScan(ctx context.Context, libraryID, scanID int64) error
}

// Server serves the Reel API over the given store.
type Server struct {
	db         *db.DB
	scanJobs   ScanRunner
	artworkDir string
	mux        *http.ServeMux
}

// New wires the routes from openapi.yaml onto a mux.
func New(d *db.DB) *Server { return NewWithRunner(d, nil) }

// NewWithRunner wires the routes with a scan enqueuer (nil = no queue).
func NewWithRunner(d *db.DB, runner ScanRunner) *Server {
	return NewWith(d, runner, "")
}

// NewWith wires the routes with a scan enqueuer (nil = no queue) and the
// on-disk artwork cache directory ("" = artwork endpoints 404).
func NewWith(d *db.DB, runner ScanRunner, artworkDir string) *Server {
	s := &Server{db: d, scanJobs: runner, artworkDir: artworkDir, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/libraries", s.handleListLibraries)
	s.mux.HandleFunc("POST /api/v1/libraries", s.handleCreateLibrary)
	s.mux.HandleFunc("GET /api/v1/libraries/{id}", s.handleGetLibrary)
	s.mux.HandleFunc("POST /api/v1/libraries/{id}/scan", s.handleStartScan)
	s.mux.HandleFunc("GET /api/v1/libraries/{id}/scan", s.handleScanStatus)
	s.mux.HandleFunc("GET /api/v1/items", s.handleListItems)
	s.mux.HandleFunc("GET /api/v1/items/{id}", s.handleGetItem)
	s.mux.HandleFunc("GET /api/v1/items/{id}/artwork/{kind}", s.handleArtwork)
	return s
}

// Artwork is the per-item artwork URLs ("field absent" when that image is
// not in the cache).
type Artwork struct {
	Poster   string `json:"poster,omitempty"`
	Backdrop string `json:"backdrop,omitempty"`
}

// ItemOut is the Item API object: the store row plus its fetched metadata
// (null until a scan has fetched it) and artwork URLs (null when no image
// is cached).
type ItemOut struct {
	db.Item
	Metadata *db.ItemMeta `json:"metadata"`
	Artwork  *Artwork     `json:"artwork"`
}

// itemOut decorates a store row with its metadata and artwork URLs.
func (s *Server) itemOut(i db.Item) ItemOut {
	o := ItemOut{Item: i}
	m, err := s.db.GetItemMeta(i.ID)
	if err != nil {
		return o // no metadata yet: both fields stay null
	}
	mm := m
	o.Metadata = &mm
	if m.PosterKey != "" || m.BackdropKey != "" {
		a := &Artwork{}
		if m.PosterKey != "" {
			a.Poster = fmt.Sprintf("/api/v1/items/%d/artwork/poster", i.ID)
		}
		if m.BackdropKey != "" {
			a.Backdrop = fmt.Sprintf("/api/v1/items/%d/artwork/backdrop", i.ID)
		}
		o.Artwork = a
	}
	return o
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

// handleStartScan: POST /api/v1/libraries/{id}/scan — create the scan
// row and enqueue the job. 409 while a scan is already queued or running
// (one scan per library at a time), 404 for an unknown library.
func (s *Server) handleStartScan(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	lib, err := s.db.GetLibrary(id)
	if errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusNotFound, "library not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.db.ActiveScan(lib.ID); err == nil {
		httpError(w, http.StatusConflict, "a scan is already queued or running")
		return
	} else if !errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	scan, err := s.db.CreateScan(lib.ID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.scanJobs != nil {
		if err := s.scanJobs.EnqueueScan(r.Context(), lib.ID, scan.ID); err != nil {
			// The queue rejected the job: make the status endpoint tell
			// the truth instead of leaving the scan queued forever.
			_ = s.db.FinishScan(scan.ID, db.ScanError, 0, 0, 0, "enqueue: "+err.Error())
			httpError(w, http.StatusBadGateway, "could not enqueue the scan")
			return
		}
	}
	writeJSON(w, http.StatusAccepted, scan)
}

// handleScanStatus: GET /api/v1/libraries/{id}/scan — the library's most
// recent scan (404 when it has never been scanned).
func (s *Server) handleScanStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	lib, err := s.db.GetLibrary(id)
	if errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusNotFound, "library not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	scan, err := s.db.LatestScan(lib.ID)
	if errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusNotFound, "library has not been scanned")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, scan)
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
	outs := make([]ItemOut, 0, len(items))
	for _, i := range items {
		outs = append(outs, s.itemOut(i))
	}
	writeJSON(w, http.StatusOK, outs)
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
	writeJSON(w, http.StatusOK, s.itemOut(i))
}

// handleArtwork: GET /api/v1/items/{id}/artwork/{kind} — serve one cached
// artwork image (kind: poster or backdrop). 400 for any other kind, 404
// when the item, its metadata or the image file is not there.
func (s *Server) handleArtwork(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if kind != "poster" && kind != "backdrop" {
		httpError(w, http.StatusBadRequest, "kind must be poster or backdrop")
		return
	}
	id, err := pathID(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.db.GetItem(id); errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusNotFound, "item not found")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	m, err := s.db.GetItemMeta(id)
	if errors.Is(err, db.ErrNotFound) {
		httpError(w, http.StatusNotFound, "item has no artwork")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	key := m.PosterKey
	if kind == "backdrop" {
		key = m.BackdropKey
	}
	if key == "" || s.artworkDir == "" {
		httpError(w, http.StatusNotFound, "artwork not cached")
		return
	}
	http.ServeFile(w, r, filepath.Join(s.artworkDir, "artwork", key))
}
