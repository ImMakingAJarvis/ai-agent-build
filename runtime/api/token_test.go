package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeTokenFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadTokenOK(t *testing.T) {
	p := writeTokenFile(t, "secret-token", 0o600)
	got, err := LoadToken(p)
	if err != nil {
		t.Fatalf("LoadToken = %v, want no error", err)
	}
	if got != "secret-token" {
		t.Errorf("LoadToken = %q, want %q", got, "secret-token")
	}
}

func TestLoadTokenTrimsNewline(t *testing.T) {
	p := writeTokenFile(t, "secret-token\n", 0o600)
	got, err := LoadToken(p)
	if err != nil {
		t.Fatalf("LoadToken = %v, want no error", err)
	}
	if got != "secret-token" {
		t.Errorf("LoadToken = %q, want %q (one trailing newline trimmed)", got, "secret-token")
	}
}

func TestLoadTokenRejectsLooseMode(t *testing.T) {
	p := writeTokenFile(t, "secret-token\n", 0o644)
	if _, err := LoadToken(p); err == nil {
		t.Fatal("LoadToken(0644 file) = nil error, want an error for group/other bits")
	}
}

func TestLoadTokenEmpty(t *testing.T) {
	for name, content := range map[string]string{
		"zero bytes":     "",
		"only a newline": "\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := writeTokenFile(t, content, 0o600)
			if _, err := LoadToken(p); err == nil {
				t.Fatal("LoadToken(empty file) = nil error, want an error")
			}
		})
	}
}

func TestLoadTokenMissing(t *testing.T) {
	if _, err := LoadToken(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("LoadToken(missing file) = nil error, want an error")
	}
}

// guardRoundTrip runs one request through Guard(token, next) and reports
// the response plus whether next was reached.
func guardRoundTrip(t *testing.T, token, method, path, auth string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "served")
	})
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	Guard(token, next).ServeHTTP(rec, req)
	return rec, called
}

func TestGuardNoHeader401(t *testing.T) {
	rec, called := guardRoundTrip(t, "secret", http.MethodGet, "/v1/tasks", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/tasks without header: status = %d, want 401", rec.Code)
	}
	if body := rec.Body.String(); body != `{"error":"unauthorized"}` {
		t.Errorf("body = %q, want %q", body, `{"error":"unauthorized"}`)
	}
	if called {
		t.Error("handler was reached without a token")
	}
}

func TestGuardWrongToken401(t *testing.T) {
	rec, called := guardRoundTrip(t, "secret", http.MethodGet, "/v1/tasks", "Bearer wrong-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/tasks with wrong token: status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("handler was reached with a wrong token")
	}
}

func TestGuardRightTokenPasses(t *testing.T) {
	rec, called := guardRoundTrip(t, "secret", http.MethodGet, "/v1/tasks", "Bearer secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/tasks with the right token: status = %d, want 200", rec.Code)
	}
	if !called {
		t.Fatal("handler was not reached with the right token")
	}
}

func TestGuardHealthzAndMetricsExempt(t *testing.T) {
	for _, path := range []string{"/healthz", "/metrics"} {
		rec, called := guardRoundTrip(t, "secret", http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s without a token: status = %d, want 200 (exempt)", path, rec.Code)
		}
		if !called {
			t.Errorf("GET %s did not reach the handler", path)
		}
	}
}

func TestGuardPostWithoutTokenIs401(t *testing.T) {
	rec, called := guardRoundTrip(t, "secret", http.MethodPost, "/v1/tasks/x/cancel", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /v1/tasks/x/cancel without a token: status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("POST /v1/tasks/x/cancel reached the handler without a token")
	}
}
