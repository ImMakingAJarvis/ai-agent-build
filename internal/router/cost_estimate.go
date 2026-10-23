// Package router provides cost estimation for API-dispatched jobs.
//
// Before any job is dispatched to a paid API (Claude Sonnet, Opus, etc.),
// EstimateCost predicts the token usage and dollar cost. The approval gate
// (Approved) blocks jobs whose estimated cost exceeds a configurable threshold.
package router

import (
	"math"
	"os"
	"strconv"
	"strings"
)

// ModelPricing holds per-model token costs.
type ModelPricing struct {
	InputPricePer1M  float64 // $ per 1M input tokens
	OutputPricePer1M float64 // $ per 1M output tokens
}

// defaultPricing contains known API pricing for Claude models.
// Update these when pricing changes.
var defaultPricing = map[string]ModelPricing{
	"claude-sonnet-5-5": {InputPricePer1M: 3.75, OutputPricePer1M: 15.00},
	"claude-opus-5-5":   {InputPricePer1M: 15.00, OutputPricePer1M: 75.00},
	"claude-haiku-5-5":  {InputPricePer1M: 0.80, OutputPricePer1M: 4.00},
	"claude-sonnet-4-20250514": {InputPricePer1M: 3.00, OutputPricePer1M: 15.00},
	"claude-opus-4-20250514":   {InputPricePer1M: 15.00, OutputPricePer1M: 75.00},
}

// CostEstimate holds the result of a cost estimation.
type CostEstimate struct {
	InputTokens  int     `json:"input_tokens"`  // Estimated input tokens
	OutputTokens int     `json:"output_tokens"` // Estimated output tokens
	TotalCost    float64 `json:"total_cost"`    // Estimated total cost in USD
	InputCost    float64 `json:"input_cost"`    // Cost attributable to input tokens
	OutputCost   float64 `json:"output_cost"`   // Cost attributable to output tokens
	Model        string  `json:"model"`         // Model that was priced
}

// EstimateCost estimates the token usage and dollar cost for a job before
// it is dispatched to an API.
//
// Input token estimation is based on the prompt length (4 chars ≈ 1 token).
// Output token estimation uses a multiplier based on job type:
//   - voice: 1.0x (short responses)
//   - fix: 2.0x (moderate responses)
//   - coding: 3.0x (long code outputs)
//   - background: 2.0x (moderate responses)
//
// For models not in the pricing table, a default of $15/M input and $75/M
// output is used (conservative Opus-level pricing).
func EstimateCost(model string, prompt string, jobType string) CostEstimate {
	pricing := getPricing(model)
	inputTokens := estimateInputTokens(prompt)
	outputTokens := estimateOutputTokens(inputTokens, jobType)

	inputCost := float64(inputTokens) / 1e6 * pricing.InputPricePer1M
	outputCost := float64(outputTokens) / 1e6 * pricing.OutputPricePer1M

	return CostEstimate{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalCost:    math.Ceil((inputCost+outputCost)*10000) / 10000, // round up to 4 decimals
		InputCost:    math.Ceil(inputCost*10000) / 10000,
		OutputCost:   math.Ceil(outputCost*10000) / 10000,
		Model:        model,
	}
}

// Approved checks whether a job should be allowed to proceed based on its
// estimated cost.
//
// Rules (in order):
//   1. If USE_LOCAL is true, always approve (no API cost).
//   2. If the job payload contains "auto_approve": "true", approve.
//   3. If estimated cost <= ROBOT_COST_THRESHOLD env var (default $1.00), approve.
//   4. Otherwise, deny — the job awaits manual approval via the dashboard or Discord.
func Approved(useLocal bool, payload map[string]string, estimate CostEstimate) bool {
	if useLocal {
		return true
	}
	if payload != nil && (payload["auto_approve"] == "true" || payload["auto_approve"] == "1") {
		return true
	}
	threshold := costThreshold()
	return estimate.TotalCost <= threshold
}

func getPricing(model string) ModelPricing {
	if p, ok := defaultPricing[model]; ok {
		return p
	}
	// Conservative default: Opus-level pricing for unknown models.
	return ModelPricing{InputPricePer1M: 15.00, OutputPricePer1M: 75.00}
}

func estimateInputTokens(prompt string) int {
	// Rule of thumb: 4 characters ≈ 1 token for English text.
	// Code may be more token-dense, but this is an estimate, not a count.
	if len(prompt) == 0 {
		return 1 // minimum 1 token
	}
	return max(1, len(prompt)/4)
}

func estimateOutputTokens(inputTokens int, jobType string) int {
	multiplier := 2.0 // default: background
	switch jobType {
	case "voice":
		multiplier = 1.0 // short conversational responses
	case "fix":
		multiplier = 2.0 // moderate responses
	case "coding":
		multiplier = 3.0 // code generation can be long
	}
	return int(math.Ceil(float64(inputTokens) * multiplier))
}

func costThreshold() float64 {
	s := os.Getenv("ROBOT_COST_THRESHOLD")
	if s == "" {
		s = "1.00"
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 1.00 // safe default
	}
	return v
}

// IsClaudeModel returns true if the model name refers to a Claude API model.
func IsClaudeModel(model string) bool {
	return strings.HasPrefix(model, "claude-")
}
