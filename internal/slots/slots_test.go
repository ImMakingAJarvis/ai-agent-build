package slots

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// slotFixtureTwoIdle is the /slots JSON for two idle slots.
const slotFixtureTwoIdle = `[
	{"id":0,"n_ctx":131072,"speculative":false,"is_processing":false,"id_task":-1,"n_prompt_tokens":0},
	{"id":1,"n_ctx":131072,"speculative":false,"is_processing":false,"id_task":-1,"n_prompt_tokens":0}
]`

// slotFixtureOneBusy is the /slots JSON for one idle and one processing slot.
const slotFixtureOneBusy = `[
	{"id":0,"n_ctx":131072,"speculative":false,"is_processing":true,"id_task":42,"n_prompt_tokens":1024},
	{"id":1,"n_ctx":131072,"speculative":false,"is_processing":false,"id_task":-1,"n_prompt_tokens":0}
]`

func TestGetSlotsTwoIdle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(slotFixtureTwoIdle))
	}))
	defer srv.Close()

	slots, err := GetSlots(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 2 {
		t.Fatalf("want 2 slots, got %d", len(slots))
	}
	for i, s := range slots {
		if s.ID != i {
			t.Errorf("slot[%d].ID = %d, want %d", i, s.ID, i)
		}
		if s.IsProcessing {
			t.Errorf("slot[%d].IsProcessing = true, want false", i)
		}
		if s.NContext != 131072 {
			t.Errorf("slot[%d].NContext = %d, want 131072", i, s.NContext)
		}
	}
}

func TestGetSlotsOneBusy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(slotFixtureOneBusy))
	}))
	defer srv.Close()

	slots, err := GetSlots(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 2 {
		t.Fatalf("want 2 slots, got %d", len(slots))
	}
	if !slots[0].IsProcessing {
		t.Error("slot 0 should be processing")
	}
	if slots[0].TaskID != 42 {
		t.Errorf("slot 0 TaskID = %d, want 42", slots[0].TaskID)
	}
	if slots[0].NPromptTokens != 1024 {
		t.Errorf("slot 0 NPromptTokens = %d, want 1024", slots[0].NPromptTokens)
	}
	if slots[1].IsProcessing {
		t.Error("slot 1 should be idle")
	}
}

func TestGetSlotsServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := GetSlots(srv.URL)
	if err == nil {
		t.Fatal("want error for 500 response")
	}
}

func TestGetSlotsBadRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	_, err := GetSlots(srv.URL)
	if err == nil {
		t.Fatal("want error for bad JSON")
	}
}

func TestClientGetSlots(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(slotFixtureOneBusy))
	}))
	defer srv.Close()

	c := NewClient()
	c.BaseURL = srv.URL
	slots, err := c.GetSlots()
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 2 {
		t.Fatalf("want 2 slots, got %d", len(slots))
	}
	if !slots[0].IsProcessing {
		t.Error("slot 0 should be processing")
	}
}

func TestUnmarshalSlotInfo(t *testing.T) {
	// Verify we can unmarshal a real llama.cpp /slots response.
	raw := `{
		"id": 0,
		"n_ctx": 131072,
		"speculative": false,
		"is_processing": true,
		"id_task": 92282,
		"n_prompt_tokens": 38052,
		"n_prompt_tokens_processed": 0,
		"params": {"temperature": 0.8, "max_tokens": 32000}
	}`
	var s SlotInfo
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if s.ID != 0 || !s.IsProcessing || s.TaskID != 92282 {
		t.Errorf("unexpected values: %+v", s)
	}
	if s.NContext != 131072 {
		t.Errorf("NContext = %d, want 131072", s.NContext)
	}
	if s.NPromptTokens != 38052 {
		t.Errorf("NPromptTokens = %d, want 38052", s.NPromptTokens)
	}
}
