package dashboard

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mapGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	return w
}

func TestMapPage(t *testing.T) {
	s := newTestServer(t)
	w := mapGet(t, s, "/map")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /map: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"JARVIS System Map", "/api/map", "/api/map/health", `href="/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("/map body missing %q", want)
		}
	}
}

func TestMapAPI(t *testing.T) {
	s := newTestServer(t)
	w := mapGet(t, s, "/api/map")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/map: %d", w.Code)
	}
	var d mapData
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(d.Problems) != 0 {
		t.Fatalf("embedded specs have problems: %v", d.Problems)
	}
	ids := map[string]bool{}
	for _, n := range d.Nodes {
		ids[n.ID] = true
	}
	for _, want := range []string{"engine", "dashboard", "chat", "agent-run", "resourced", "rabbitmq", "discord"} {
		if !ids[want] {
			t.Errorf("node %q missing from /api/map", want)
		}
	}
	if len(d.Edges) == 0 {
		t.Fatal("no edges")
	}
	for _, e := range d.Edges {
		if !ids[e.From] || !ids[e.To] {
			t.Errorf("edge %s → %s points outside the node set", e.From, e.To)
		}
	}
	found := false
	for _, e := range d.Edges {
		if e.From == "chat" && e.To == "engine" {
			found = true
		}
	}
	if !found {
		t.Error("expected the chat → engine edge")
	}
}

func TestMapSpecSource(t *testing.T) {
	s := newTestServer(t)
	w := mapGet(t, s, "/api/map/spec/engine")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "## Verify live") {
		t.Fatalf("GET /api/map/spec/engine: %d %.200s", w.Code, w.Body.String())
	}
	if w := mapGet(t, s, "/api/map/spec/nope"); w.Code != http.StatusNotFound {
		t.Errorf("unknown spec: %d, want 404", w.Code)
	}
	if w := mapGet(t, s, "/api/map/spec/README"); w.Code != http.StatusNotFound {
		t.Errorf("README is not a spec id: %d, want 404", w.Code)
	}
}

func TestMapHealthUsesProbe(t *testing.T) {
	s := newTestServer(t)
	old := mapProbe
	defer func() { mapProbe = old }()
	mapHealthMu.Lock()
	mapHealthAt = time.Time{}
	mapHealthMu.Unlock()

	var probed []string
	mapProbe = func(ctx context.Context, target string) probeResult {
		probed = append(probed, target)
		if strings.Contains(target, "engine:11434") {
			return probeResult{State: "down", Detail: "connection refused"}
		}
		return probeResult{State: "up", Detail: "HTTP 200"}
	}
	w := mapGet(t, s, "/api/map/health")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/map/health: %d", w.Code)
	}
	var h mapHealth
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.Results["engine"].State != "down" || h.Results["memory"].State != "up" {
		t.Fatalf("results = %+v", h.Results)
	}
	if _, ok := h.Results["discord"]; ok {
		t.Error("an external spec without a health target was probed")
	}
	if len(probed) == 0 {
		t.Fatal("probe never called")
	}
}

func TestProbeTarget(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if r := probeTarget(ctx, ok.URL); r.State != "up" {
		t.Errorf("200 → %+v", r)
	}
	if r := probeTarget(ctx, bad.URL); r.State != "down" || r.Detail != "HTTP 503" {
		t.Errorf("503 → %+v", r)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if r := probeTarget(ctx, "tcp://"+addr); r.State != "up" {
		t.Errorf("tcp open → %+v", r)
	}
	ln.Close()
	if r := probeTarget(ctx, "tcp://"+addr); r.State != "down" {
		t.Errorf("tcp closed → %+v", r)
	}
}

func TestQueueSiteLinksToMap(t *testing.T) {
	s := newTestServer(t)
	w := mapGet(t, s, "/")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `href="/map"`) {
		t.Fatalf("queue site does not link to /map (status %d)", w.Code)
	}
}
