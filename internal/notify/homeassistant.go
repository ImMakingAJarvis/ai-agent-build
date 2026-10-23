// Package notify sends notifications to external services.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	defaultHAURL      = "http://homeassistant.local:8123"
	defaultService    = "notify.persistent_notification"
	defaultTimeout    = 10 * time.Second
)

// Client wraps HTTP calls to the Home Assistant REST API.
type Client struct {
	baseURL  string
	token    string
	service  string
	http     *http.Client
}

// NewClient creates a Home Assistant notification client.
// Reads HA_URL, HA_TOKEN, and HA_NOTIFY_SERVICE from environment.
func NewClient() *Client {
	url := os.Getenv("HA_URL")
	if url == "" {
		url = defaultHAURL
	}
	token := os.Getenv("HA_TOKEN")
	service := os.Getenv("HA_NOTIFY_SERVICE")
	if service == "" {
		service = defaultService
	}
	return &Client{
		baseURL: url,
		token:   token,
		service: service,
		http: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

// Configured returns true if the client has a real token to work with.
func (c *Client) Configured() bool {
	return c.token != ""
}

// NotifyRequest is the body sent to Home Assistant's notify service.
type NotifyRequest struct {
	Title   string `json:"title"`
	Message string `json:"message"`
	Data    map[string]string `json:"data,omitempty"`
}

// Send posts a notification to the configured Home Assistant notify service.
func (c *Client) Send(title, message string) error {
	return c.SendWith(title, message, nil)
}

// SendWith posts a notification with optional extra data fields.
func (c *Client) SendWith(title, message string, data map[string]string) error {
	if !c.Configured() {
		return fmt.Errorf("home assistant: not configured (HA_TOKEN is empty)")
	}

	req := NotifyRequest{
		Title:   title,
		Message: message,
		Data:    data,
	}
	return c.do(c.service, req)
}

// SendTo posts a notification to a specific Home Assistant notify service
// (e.g. "notify.mobile_app_<device>").
func (c *Client) SendTo(service, title, message string) error {
	if !c.Configured() {
		return fmt.Errorf("home assistant: not configured (HA_TOKEN is empty)")
	}

	req := NotifyRequest{
		Title:   title,
		Message: message,
	}
	return c.do(service, req)
}

// do executes a single notification POST against the HA REST API.
func (c *Client) do(service string, req NotifyRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal notify request: %w", err)
	}

	url := c.baseURL + "/api/services/" + service
	r, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(r)
	if err != nil {
		return fmt.Errorf("POST %s: %w", service, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s returned %d: %s", service, resp.StatusCode, b)
	}
	return nil
}
