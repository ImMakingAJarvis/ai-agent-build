package queue

import (
	"testing"
	"time"
)

// ---- Scheduler benchmarks ----

func BenchmarkScheduler_SetDepths(b *testing.B) {
	s := NewScheduler()
	depths := map[string]int{
		VoiceQueue:      3,
		CodingQueue:     7,
		BackgroundQueue: 12,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.SetDepths(depths)
	}
}

func BenchmarkScheduler_SetDepths_VoiceOnly(b *testing.B) {
	s := NewScheduler()
	depths := map[string]int{
		VoiceQueue:      1,
		CodingQueue:     0,
		BackgroundQueue: 0,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.SetDepths(depths)
	}
}

func BenchmarkScheduler_SetDepths_AllIdle(b *testing.B) {
	s := NewScheduler()
	depths := map[string]int{
		VoiceQueue:      0,
		CodingQueue:     0,
		BackgroundQueue: 0,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.SetDepths(depths)
	}
}

func BenchmarkScheduler_IsPaused(b *testing.B) {
	s := NewScheduler()
	depths := map[string]int{
		VoiceQueue:      2,
		CodingQueue:     5,
		BackgroundQueue: 0,
	}
	s.SetDepths(depths)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.IsPaused(VoiceQueue)
	}
}

func BenchmarkScheduler_IsPaused_Parallel(b *testing.B) {
	s := NewScheduler()
	depths := map[string]int{
		VoiceQueue:      1,
		CodingQueue:     3,
		BackgroundQueue: 8,
	}
	s.SetDepths(depths)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = s.IsPaused(CodingQueue)
		}
	})
}

func BenchmarkScheduler_ConcurrentSetDepthsAndIsPaused(b *testing.B) {
	s := NewScheduler()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		depths := map[string]int{
			VoiceQueue:      1,
			CodingQueue:     2,
			BackgroundQueue: 3,
		}
		for pb.Next() {
			s.SetDepths(depths)
		}
	})
}

// ---- Message encoding/decoding benchmarks ----

func BenchmarkEncodeMessage(b *testing.B) {
	msg := Message{
		ID:        "test-job-001",
		Type:      "coding",
		Priority:  5,
		Payload: map[string]string{
			"prompt":  "Write a Go function that reverses a string efficiently.",
			"context": "The user is working on a CLI tool.",
		},
		CreatedAt: time.Now(),
		Source:    "cli",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = encodeMessage(msg)
	}
}

func BenchmarkDecodeMessage(b *testing.B) {
	msg := Message{
		ID:        "test-job-001",
		Type:      "coding",
		Priority:  5,
		Payload: map[string]string{
			"prompt":  "Write a Go function that reverses a string efficiently.",
			"context": "The user is working on a CLI tool.",
		},
		CreatedAt: time.Now(),
		Source:    "cli",
	}
	body, err := encodeMessage(msg)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = DecodeMessage(body)
	}
}

func BenchmarkEncodeDecodeRoundTrip(b *testing.B) {
	msg := Message{
		ID:            "round-trip-001",
		Type:          "voice",
		Priority:      10,
		Payload:       map[string]string{"prompt": "What is the weather today?"},
		CreatedAt:     time.Now(),
		Source:        "voice",
		ForceAPI:      true,
		ModelOverride: "claude-sonnet-5-5",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body, _ := encodeMessage(msg)
		_, _ = DecodeMessage(body)
	}
}

// ---- QueueForType benchmarks ----

func BenchmarkQueueForType(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = QueueForType("coding")
	}
}

// ---- History benchmarks ----

func BenchmarkHistory_Start(b *testing.B) {
	h, err := NewHistory()
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close()

	msg := Message{
		ID:      "bench-start-001",
		Type:    "background",
		Payload: map[string]string{"prompt": "benchmark test"},
		Source:  "cli",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msg.ID = string(rune('A'+i%26)) + "-bench"
		h.Start(msg, "jarvis-brain", true, BackgroundQueue)
	}
}

func BenchmarkHistory_Complete(b *testing.B) {
	h, err := NewHistory()
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close()

	msg := Message{
		ID:      "bench-complete-001",
		Type:    "coding",
		Payload: map[string]string{"prompt": "benchmark test"},
		Source:  "cli",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := h.Start(msg, "jarvis-brain", true, CodingQueue)
		h.Complete(rec)
	}
}

func BenchmarkHistory_Fail(b *testing.B) {
	h, err := NewHistory()
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close()

	msg := Message{
		ID:      "bench-fail-001",
		Type:    "voice",
		Payload: map[string]string{"prompt": "benchmark test"},
		Source:  "discord",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := h.Start(msg, "claude-sonnet-5-5", false, VoiceQueue)
		h.Fail(rec, testErr{})
	}
}

func BenchmarkHistory_Denied(b *testing.B) {
	h, err := NewHistory()
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close()

	msg := Message{
		ID:      "bench-denied-001",
		Type:    "coding",
		Payload: map[string]string{"prompt": "benchmark test"},
		Source:  "github",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := h.Start(msg, "claude-opus-5-5", false, CodingQueue)
		h.Denied(rec, 2.50)
	}
}

// testErr is a simple sentinel error for benchmark use.
type testErr struct{}

func (testErr) Error() string { return "benchmark test error" }
