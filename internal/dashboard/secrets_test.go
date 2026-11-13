package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newSecretsServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{secrets: initSecrets(t.TempDir())}
	return s
}

func postSecret(t *testing.T, s *Server, name, value string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(secretsAddRequest{Name: name, Value: value})
	req := httptest.NewRequest("POST", "/api/secrets", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.secrets.handleSecretsAdd(w, req)
	return w
}

func TestSecretsAddListRemove(t *testing.T) {
	s := newSecretsServer(t)

	// Add two secrets.
	if w := postSecret(t, s, "api_key", "sk-live-abc"); w.Code != http.StatusOK {
		t.Fatalf("add api_key: %d %s", w.Code, w.Body.String())
	}
	if w := postSecret(t, s, "db_pass", "hunter2"); w.Code != http.StatusOK {
		t.Fatalf("add db_pass: %d %s", w.Code, w.Body.String())
	}

	// List shows both names and metadata only.
	w := httptest.NewRecorder()
	s.secrets.handleSecretsList(w, httptest.NewRequest("GET", "/api/secrets", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2, got %d", len(list))
	}
	for _, key := range []string{"name", "bytes", "updated"} {
		if _, ok := list[0][key]; !ok {
			t.Fatalf("list entry missing %q: %v", key, list[0])
		}
	}
	if got := list[0]["name"]; got != "api_key" {
		t.Fatalf("first name = %v", got)
	}

	// The list payload must not contain either value.
	body := w.Body.String()
	for _, v := range []string{"sk-live-abc", "hunter2"} {
		if strings.Contains(body, v) {
			t.Fatalf("list response leaked value %q", v)
		}
	}

	// Remove one.
	req := httptest.NewRequest("DELETE", "/api/secrets/api_key", nil)
	req.SetPathValue("name", "api_key")
	w = httptest.NewRecorder()
	s.secrets.handleSecretsRemove(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", w.Code, w.Body.String())
	}

	// Removing again is a 404.
	req = httptest.NewRequest("DELETE", "/api/secrets/api_key", nil)
	req.SetPathValue("name", "api_key")
	w = httptest.NewRecorder()
	s.secrets.handleSecretsRemove(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("re-remove: want 404, got %d", w.Code)
	}

	// The other one survives.
	w = httptest.NewRecorder()
	s.secrets.handleSecretsList(w, httptest.NewRequest("GET", "/api/secrets", nil))
	var list2 []map[string]any
	json.Unmarshal(w.Body.Bytes(), &list2)
	if len(list2) != 1 || list2[0]["name"] != "db_pass" {
		t.Fatalf("after remove: %v", list2)
	}
}

func TestSecretsAddValidation(t *testing.T) {
	s := newSecretsServer(t)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"traversal", `{"name":"../../etc","value":"x"}`},
		{"empty name", `{"name":"","value":"x"}`},
		{"space", `{"name":"a b","value":"x"}`},
		{"not json", `this is not json`},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/secrets", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		s.secrets.handleSecretsAdd(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("add %s: want 400, got %d (%s)", tc.name, w.Code, w.Body.String())
		}
	}
}

func TestSecretsRemoveValidation(t *testing.T) {
	s := newSecretsServer(t)
	for _, name := range []string{"a/b", "../etc", "", "a b"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("DELETE", "/api/secrets/x", nil)
		req.SetPathValue("name", name)
		s.secrets.handleSecretsRemove(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("remove %q: want 400, got %d (%s)", name, w.Code, w.Body.String())
		}
	}
}

func TestSecretsPageRenders(t *testing.T) {
	s := newSecretsServer(t)
	tmpl, err := parseTemplates(t)
	if err != nil {
		t.Fatal(err)
	}
	s.tmpl = tmpl
	w := httptest.NewRecorder()
	s.secrets.handleSecretsPage(s)(w, httptest.NewRequest("GET", "/secrets", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("page: %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Write-only") {
		t.Fatal("page missing the write-only notice")
	}
	if !strings.Contains(body, "/api/secrets") {
		t.Fatal("page does not reference the API")
	}
}

func TestNoEndpointReturnsValue(t *testing.T) {
	s := newSecretsServer(t)
	if w := postSecret(t, s, "token", "top-secret-value-xyz"); w.Code != http.StatusOK {
		t.Fatalf("add: %d", w.Code)
	}
	// Walk every secrets endpoint and make sure the value never appears.
	reqs := []struct {
		method, path string
		handler      func(w http.ResponseWriter, r *http.Request)
	}{
		{"GET", "/api/secrets", s.secrets.handleSecretsList},
		{"DELETE", "/api/secrets/token-then-readd", s.secrets.handleSecretsRemove},
	}
	for _, q := range reqs {
		w := httptest.NewRecorder()
		q.handler(w, httptest.NewRequest(q.method, q.path, nil))
		if strings.Contains(w.Body.String(), "top-secret-value-xyz") {
			t.Fatalf("%s %s leaked the value", q.method, q.path)
		}
	}
}
