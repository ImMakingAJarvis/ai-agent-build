// Package notify tests
package notify

import (
	"encoding/json"
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

func TestGetState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s; want GET", r.Method)
		}
		if r.URL.Path != "/api/states/person.owner" {
			t.Errorf("path = %q; want /api/states/person.owner", r.URL.Path)
		}
		resp := HAEntity{EntityID: "person.owner", State: "home"}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := &Client{
		baseURL: server.URL,
		token:   "test-token",
		http:    http.DefaultClient,
	}
	entity, err := c.GetState("person.owner")
	if err != nil {
		t.Fatalf("GetState() = %v; want nil", err)
	}
	if entity.EntityID != "person.owner" {
		t.Errorf("EntityID = %q; want person.owner", entity.EntityID)
	}
	if entity.State != "home" {
		t.Errorf("State = %q; want home", entity.State)
	}
}

func TestGetStateUnconfigured(t *testing.T) {
	c := &Client{}
	_, err := c.GetState("person.owner")
	if err == nil {
		t.Error("expected error for unconfigured client")
	}
}

func TestListStates(t *testing.T) {
	entities := []HAEntity{
		{EntityID: "media_player.living_room", State: "playing"},
		{EntityID: "media_player.bedroom", State: "idle"},
		{EntityID: "person.owner", State: "home"},
		{EntityID: "input_boolean.job_announce", State: "on"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(entities)
	}))
	defer server.Close()

	c := &Client{
		baseURL: server.URL,
		token:   "test-token",
		http:    http.DefaultClient,
	}
	result, err := c.ListStates()
	if err != nil {
		t.Fatalf("ListStates() = %v; want nil", err)
	}
	if len(result) != 4 {
		t.Errorf("got %d entities; want 4", len(result))
	}
}

func TestFindMediaPlayers(t *testing.T) {
	entities := []HAEntity{
		{EntityID: "media_player.living_room", State: "playing"},
		{EntityID: "media_player.bedroom", State: "idle"},
		{EntityID: "media_player.kitchen", State: "idle"},
		{EntityID: "person.owner", State: "home"},
		{EntityID: "light.ceiling", State: "on"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(entities)
	}))
	defer server.Close()

	c := &Client{
		baseURL: server.URL,
		token:   "test-token",
		http:    http.DefaultClient,
	}
	players, err := c.FindMediaPlayers()
	if err != nil {
		t.Fatalf("FindMediaPlayers() = %v; want nil", err)
	}
	if len(players) != 3 {
		t.Fatalf("got %d players; want 3", len(players))
	}
	expected := map[string]bool{
		"media_player.living_room": true,
		"media_player.bedroom":     true,
		"media_player.kitchen":     true,
	}
	for _, p := range players {
		if !expected[p] {
			t.Errorf("unexpected player %q", p)
		}
	}
}

func TestTTSTo(t *testing.T) {
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := &Client{
		baseURL: server.URL,
		token:   "test-token",
		http:    http.DefaultClient,
	}
	err := c.TTSTo("media_player.living_room", "Hello world")
	if err != nil {
		t.Fatalf("TTSTo() = %v; want nil", err)
	}
	if gotBody["entity_id"] != "media_player.living_room" {
		t.Errorf("entity_id = %q; want media_player.living_room", gotBody["entity_id"])
	}
	if gotBody["message"] != "Hello world" {
		t.Errorf("message = %q; want Hello world", gotBody["message"])
	}
}

func TestTTSToAll(t *testing.T) {
	callCount := 0
	entities := []HAEntity{
		{EntityID: "media_player.living_room", State: "playing"},
		{EntityID: "media_player.bedroom", State: "idle"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		// First call is ListStates, subsequent calls are TTS.
		if r.URL.Path == "/api/states" {
			json.NewEncoder(w).Encode(entities)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := &Client{
		baseURL: server.URL,
		token:   "test-token",
		http:    http.DefaultClient,
	}
	err := c.TTSToAll("Test announcement")
	if err != nil {
		t.Fatalf("TTSToAll() = %v; want nil", err)
	}
	// 1 ListStates call + 2 TTS calls (one per player)
	if callCount != 3 {
		t.Errorf("expected 3 calls (1 list + 2 tts), got %d", callCount)
	}
}

func TestTTSToAllNoPlayers(t *testing.T) {
	entities := []HAEntity{
		{EntityID: "person.owner", State: "home"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(entities)
	}))
	defer server.Close()

	c := &Client{
		baseURL: server.URL,
		token:   "test-token",
		http:    http.DefaultClient,
	}
	err := c.TTSToAll("Test")
	if err == nil {
		t.Error("expected error when no media players found")
	}
}

func TestCallService(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := &Client{
		baseURL: server.URL,
		token:   "test-token",
		http:    http.DefaultClient,
	}
	err := c.CallService("input_boolean.turn_on", map[string]any{
		"entity_id": "input_boolean.job_announce",
	})
	if err != nil {
		t.Fatalf("CallService() = %v; want nil", err)
	}
	if gotBody["entity_id"] != "input_boolean.job_announce" {
		t.Errorf("entity_id = %v; want input_boolean.job_announce", gotBody["entity_id"])
	}
}

func TestCallServiceUnconfigured(t *testing.T) {
	c := &Client{}
	err := c.CallService("input_boolean.turn_on", map[string]any{})
	if err == nil {
		t.Error("expected error for unconfigured client")
	}
}
