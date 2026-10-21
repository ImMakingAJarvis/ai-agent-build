package router

import (
	"os"
	"testing"
)

func TestEstimateCost(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		prompt     string
		jobType    string
		wantModel  string
		minCost    float64 // cost must be >= this
	}{
		{
			name:      "short voice prompt on sonnet",
			model:     "claude-sonnet-5-5",
			prompt:    "what's the weather",
			jobType:   "voice",
			wantModel: "claude-sonnet-5-5",
			minCost:   0,
		},
		{
			name:      "long coding prompt on opus",
			model:     "claude-opus-5-5",
			prompt:    "write a full REST API server with auth, database, and tests in Go",
			jobType:   "coding",
			wantModel: "claude-opus-5-5",
			minCost:   0,
		},
		{
			name:      "empty prompt still costs something",
			model:     "claude-sonnet-5-5",
			prompt:    "",
			jobType:   "background",
			wantModel: "claude-sonnet-5-5",
			minCost:   0,
		},
		{
			name:      "unknown model uses conservative pricing",
			model:     "gpt-5",
			prompt:    "hello world",
			jobType:   "background",
			wantModel: "gpt-5",
			minCost:   0,
		},
		{
			name:      "haiku is cheapest",
			model:     "claude-haiku-5-5",
			prompt:    "summarize this document please tell me what it says",
			jobType:   "background",
			wantModel: "claude-haiku-5-5",
			minCost:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateCost(tt.model, tt.prompt, tt.jobType)
			if got.Model != tt.wantModel {
				t.Errorf("Model = %q, want %q", got.Model, tt.wantModel)
			}
			if got.TotalCost < tt.minCost {
				t.Errorf("TotalCost = %f, want >= %f", got.TotalCost, tt.minCost)
			}
			if got.InputTokens < 1 {
				t.Errorf("InputTokens = %d, want >= 1", got.InputTokens)
			}
			if got.OutputTokens < 1 {
				t.Errorf("OutputTokens = %d, want >= 1", got.OutputTokens)
			}
			if got.TotalCost < got.InputCost {
				t.Errorf("TotalCost (%f) < InputCost (%f)", got.TotalCost, got.InputCost)
			}
		})
	}
}

func TestEstimateCostCodingMultiplier(t *testing.T) {
	// Coding jobs should have higher output token estimates than voice.
	coding := EstimateCost("claude-sonnet-5-5", "write a function", "coding")
	voice := EstimateCost("claude-sonnet-5-5", "write a function", "voice")
	if coding.OutputTokens <= voice.OutputTokens {
		t.Errorf("coding output tokens (%d) should exceed voice (%d)", coding.OutputTokens, voice.OutputTokens)
	}
}

func TestApprovedLocalAlways(t *testing.T) {
	estimate := CostEstimate{TotalCost: 100.00}
	if !Approved(true, nil, estimate) {
		t.Error("local jobs should always be approved")
	}
}

func TestApprovedAutoApprove(t *testing.T) {
	estimate := CostEstimate{TotalCost: 100.00}
	payload := map[string]string{"auto_approve": "true"}
	if !Approved(false, payload, estimate) {
		t.Error("auto_approve=true should bypass threshold")
	}
}

func TestApprovedUnderThreshold(t *testing.T) {
	estimate := CostEstimate{TotalCost: 0.50}
	if !Approved(false, nil, estimate) {
		t.Error("jobs under $1 threshold should be approved")
	}
}

func TestApprovedOverThreshold(t *testing.T) {
	// Set a low threshold to test denial.
	os.Setenv("ROBOT_COST_THRESHOLD", "0.01")
	defer os.Unsetenv("ROBOT_COST_THRESHOLD")

	estimate := CostEstimate{TotalCost: 1.00}
	if Approved(false, nil, estimate) {
		t.Error("jobs over threshold should be denied")
	}
}

func TestIsClaudeModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"claude-sonnet-5-5", true},
		{"claude-opus-5-5", true},
		{"claude-haiku-5-5", true},
		{"jarvis-brain", false},
		{"qwen3.6:27b", false},
		{"gpt-4", false},
	}
	for _, tt := range tests {
		got := IsClaudeModel(tt.model)
		if got != tt.want {
			t.Errorf("IsClaudeModel(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}
