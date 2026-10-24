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

// HAEntity represents a Home Assistant entity state response.
type HAEntity struct {
	EntityID string `json:"entity_id"`
	State    string `json:"state"`
	Attributes map[string]json.RawMessage `json:"attributes,omitempty"`
}

// GetState fetches the current state of a Home Assistant entity.
func (c *Client) GetState(entityID string) (*HAEntity, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("home assistant: not configured (HA_TOKEN is empty)")
	}

	url := c.baseURL + "/api/states/" + entityID
	r, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	r.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(r)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", entityID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("entity %s returned %d: %s", entityID, resp.StatusCode, b)
	}

	var entity HAEntity
	if err := json.NewDecoder(resp.Body).Decode(&entity); err != nil {
		return nil, fmt.Errorf("decode entity %s: %w", entityID, err)
	}
	return &entity, nil
}

// ListStates fetches all entity states from Home Assistant.
func (c *Client) ListStates() ([]HAEntity, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("home assistant: not configured (HA_TOKEN is empty)")
	}

	url := c.baseURL + "/api/states"
	r, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	r.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(r)
	if err != nil {
		return nil, fmt.Errorf("GET states: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("states returned %d: %s", resp.StatusCode, b)
	}

	var entities []HAEntity
	if err := json.NewDecoder(resp.Body).Decode(&entities); err != nil {
		return nil, fmt.Errorf("decode states: %w", err)
	}
	return entities, nil
}

// FindMediaPlayers returns all media_player entities.
func (c *Client) FindMediaPlayers() ([]string, error) {
	entities, err := c.ListStates()
	if err != nil {
		return nil, err
	}
	var players []string
	for _, e := range entities {
		if len(e.EntityID) >= 13 && e.EntityID[:13] == "media_player." {
			players = append(players, e.EntityID)
		}
	}
	return players, nil
}

// TTSTo sends a text-to-speech announcement to a specific media player entity.
// Uses the default TTS platform configured in HA (google, microsoft, or native).
func (c *Client) TTSTo(entityID, text string) error {
	if !c.Configured() {
		return fmt.Errorf("home assistant: not configured (HA_TOKEN is empty)")
	}

	// Try TTS platforms in order of preference.
	platforms := []string{"google_say", "microsoft", "tts.cloud"}
	for _, platform := range platforms {
		reqBody := map[string]string{
			"entity_id": entityID,
			"message":   text,
		}
		body, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal TTS request: %w", err)
		}

		url := c.baseURL + "/api/services/" + platform
		r, err := http.NewRequest("POST", url, bytes.NewReader(body))
		if err != nil {
			continue
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+c.token)

		resp, err := c.http.Do(r)
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
			return nil
		}
		// Platform may not be available; try the next one.
	}

	// If all platforms failed, return an error with the last attempt's info.
	return fmt.Errorf("tts: all platforms failed for entity %s", entityID)
}

// TTSToAll sends a text-to-speech announcement to all media players.
func (c *Client) TTSToAll(text string) error {
	players, err := c.FindMediaPlayers()
	if err != nil {
		return fmt.Errorf("find media players: %w", err)
	}
	if len(players) == 0 {
		return fmt.Errorf("tts: no media players found")
	}
	var lastErr error
	for _, p := range players {
		if err := c.TTSTo(p, text); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// CallService calls an arbitrary Home Assistant service.
func (c *Client) CallService(serviceName string, data map[string]any) error {
	if !c.Configured() {
		return fmt.Errorf("home assistant: not configured (HA_TOKEN is empty)")
	}

	body, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal service data: %w", err)
	}

	url := c.baseURL + "/api/services/" + serviceName
	r, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(r)
	if err != nil {
		return fmt.Errorf("POST %s: %w", serviceName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s returned %d: %s", serviceName, resp.StatusCode, b)
	}
	return nil
}
