// Package notify tests
package notify

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestNewClientDefaults(t *testing.T) {
	// Clear env vars to test defaults.
	os.Unsetenv("HA_URL")
	os.Unsetenv("HA_TOKEN")
	os.Unsetenv("HA_NOTIFY_SERVICE")

	c := NewClient()
	if c.baseURL != defaultHAURL {
		t.Errorf("baseURL = %q; want %q", c.baseURL, defaultHAURL)
	}
	if c.service != defaultService {
		t.Errorf("service = %q; want %q", c.service, defaultService)
	}
	if c.Configured() {
		t.Error("Configured() = true with no token")
	}
}

func TestNewClientEnvVars(t *testing.T) {
	os.Setenv("HA_URL", "http://my-ha:8123")
	os.Setenv("HA_TOKEN", "test-token-123")
	os.Setenv("HA_NOTIFY_SERVICE", "notify.mobile_app_myphone")
	defer func() {
		os.Unsetenv("HA_URL")
		os.Unsetenv("HA_TOKEN")
		os.Unsetenv("HA_NOTIFY_SERVICE")
	}()

	c := NewClient()
	if c.baseURL != "http://my-ha:8123" {
		t.Errorf("baseURL = %q; want %q", c.baseURL, "http://my-ha:8123")
	}
	if c.token != "test-token-123" {
		t.Errorf("token = %q; want %q", c.token, "test-token-123")
	}
	if c.service != "notify.mobile_app_myphone" {
		t.Errorf("service = %q; want %q", c.service, "notify.mobile_app_myphone")
	}
	if !c.Configured() {
		t.Error("Configured() = false with token set")
	}
}

func TestSendUnconfigured(t *testing.T) {
	c := &Client{}
	err := c.Send("title", "message")
	if err == nil {
		t.Error("expected error for unconfigured client")
	}
}

func TestSendSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s; want POST", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("missing Bearer token in Authorization header")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := &Client{
		baseURL: server.URL,
		token:   "test-token",
		service: "notify.test",
		http:    http.DefaultClient,
	}
	err := c.Send("Hello", "World")
	if err != nil {
		t.Fatalf("Send() = %v; want nil", err)
	}
}

func TestSendTo(t *testing.T) {
	var gotService string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotService = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := &Client{
		baseURL: server.URL,
		token:   "test-token",
		service: "notify.default",
		http:    http.DefaultClient,
	}
	err := c.SendTo("notify.mobile_app_phone", "Test", "Body")
	if err != nil {
		t.Fatalf("SendTo() = %v; want nil", err)
	}
	if gotService != "/api/services/notify.mobile_app_phone" {
		t.Errorf("path = %q; want /api/services/notify.mobile_app_phone", gotService)
	}
}
