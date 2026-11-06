package api_test

// Contract test: boots the server, walks openapi.yaml, and checks the
// request/response shape of every endpoint in the spec.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/api"
	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/db"
)

type specDoc struct {
	Paths map[string]map[string]any `yaml:"paths"`
}

// contractCase is the expected behaviour for one (method, path) of the spec.
type contractCase struct {
	ReqPath string // concrete request path (path params substituted)
	Query   string
	Body    string
	Want    int
	Fields  []string // required on the object (or first array element)
}

var contract = map[string]contractCase{
	"GET /health":                   {Want: 200, Fields: []string{"status", "version"}},
	"GET /api/v1/libraries":         {Want: 200},
	"POST /api/v1/libraries":        {Body: `{"name":"CI Movies","path":"/data/ci","type":"movies"}`, Want: 201, Fields: []string{"id", "name", "path", "type"}},
	"GET /api/v1/libraries/{id}":    {ReqPath: "/api/v1/libraries/1", Want: 200, Fields: []string{"id", "name", "path", "type"}},
	"GET /api/v1/items":             {Query: "library_id=1&type=movie", Want: 200, Fields: []string{"id", "library_id", "type", "title"}},
	"GET /api/v1/items/{id}":        {ReqPath: "/api/v1/items/1", Want: 200, Fields: []string{"id", "library_id", "type", "title"}},
}

func openSpec(t *testing.T) specDoc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc specDoc
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return doc
}

func checkShape(t *testing.T, body []byte, fields []string) {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("response is not JSON: %s", body)
	}
	if fields == nil {
		return
	}
	obj, ok := v.(map[string]any)
	if arr, isArr := v.([]any); isArr && len(arr) > 0 {
		obj, ok = arr[0].(map[string]any)
	}
	if !ok {
		t.Fatalf("response is not an object or non-empty array of objects: %s", body)
	}
	for _, f := range fields {
		if _, ok := obj[f]; !ok {
			t.Fatalf("response missing field %q: %s", f, body)
		}
	}
}

func TestContract(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "reel.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = d.Close() }()
	libMovies, _ := d.CreateLibrary(db.Library{Name: "Movies", Path: "/data/movies", Type: "movies"})
	libShows, _ := d.CreateLibrary(db.Library{Name: "Shows", Path: "/data/shows", Type: "shows"})
	_, _ = d.InsertItem(db.Item{LibraryID: libMovies.ID, Type: "movie", Title: "Big Buck Bunny", Path: "/bbb.mkv"})
	season, _ := d.InsertItem(db.Item{LibraryID: libShows.ID, Type: "season", Title: "Season 1", Path: "/s1"})
	_, _ = d.InsertItem(db.Item{LibraryID: libShows.ID, Type: "episode", Title: "Episode 1", Path: "/e1", ParentID: &season.ID})

	ts := httptest.NewServer(api.New(d).Handler())
	defer ts.Close()

	doc := openSpec(t)
	for path, ops := range doc.Paths {
		for method := range ops {
			key := strings.ToUpper(method) + " " + path
			cc, ok := contract[key]
			if !ok {
				t.Fatalf("spec endpoint %s has no contract case", key)
			}
			reqPath := cc.ReqPath
			if reqPath == "" {
				reqPath = path
			}
			var body io.Reader
			if cc.Body != "" {
				body = bytes.NewReader([]byte(cc.Body))
			}
			req, err := http.NewRequest(strings.ToUpper(method), ts.URL+reqPath+"?"+strings.TrimPrefix(cc.Query, "?"), body)
			if err != nil {
				t.Fatal(err)
			}
			if cc.Body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s: %v", key, err)
			}
			b, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if res.StatusCode != cc.Want {
				t.Fatalf("%s: status %d, want %d (body %s)", key, res.StatusCode, cc.Want, b)
			}
			checkShape(t, b, cc.Fields)
		}
	}

	// Negative shapes: 404 and 400 carry the spec's Error object.
	for _, tc := range []struct{ method, path string; want int; field string }{
		{"GET", "/api/v1/libraries/999999", 404, "error"},
		{"GET", "/api/v1/items?type=banana", 400, "error"},
	} {
		res, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != tc.want {
			t.Fatalf("%s %s: status %d, want %d", tc.method, tc.path, res.StatusCode, tc.want)
		}
		checkShape(t, b, []string{tc.field})
	}
}
