package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// moeGet runs GET through the server's real route table and returns the body.
func moeGet(t *testing.T, s *Server, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", path, w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("GET %s: Content-Type = %q", path, ct)
	}
	return w.Body.String()
}

// TestMoeBenchPage is the data-transcription test: every key figure the page
// claims must match its source doc (docs/moe-benchmark.md,
// docs/benchmarks/*) verbatim.
func TestMoeBenchPage(t *testing.T) {
	s := newTestServer(t)
	body := moeGet(t, s, "/benchmarks")

	for _, want := range []string{
		// Ollama A/B (docs/moe-benchmark.md)
		"212.1", "102.9", "163,621", "44.52", "58.16", "2.06",
		"49,152", "65,536", "300 MB",
		// Qwen 3.8 Flash Next sweep
		"0.9825", "137", "259", "105.4", "44.5", "665.7", "52.3", "7.14",
		"4,501", "8,256", "no report produced",
		// GLM 5.3 flash sweep
		"0.975", "0.972", "0.970", "344", "317.31", "256 MiB",
		"101.8", "9,429,984", "49,896,298,080",
		// GLM bandwidth bench
		"28.6", "38.8", "25.3", "52.9",
		// Qwen Ollama pull attempt
		"cloud-only", "125b-mlx",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestMoeBenchPageLinksAndSources: the page links back to the dashboard and
// cites all three source-doc families.
func TestMoeBenchPageLinksAndSources(t *testing.T) {
	s := newTestServer(t)
	body := moeGet(t, s, "/benchmarks")

	for _, want := range []string{
		`href="/"`, "moe-benchmark.md", "qwen38-flash-next", "glm53-flash",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}
