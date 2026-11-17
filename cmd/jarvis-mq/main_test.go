package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// TestBuildSubmitMessage pins the envelope-v2 request `submit` publishes
// (plan §3.3): every v2 field lands in the message, the envelope is marked
// v2, and Resources is nil unless a hint was given (a worker then uses its
// own defaults).
func TestBuildSubmitMessage(t *testing.T) {
	dl := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	m, err := buildSubmitMessage(submitParams{
		capability: "image.generate",
		id:         "test-submit-1",
		replyTo:    "flow.plan-x",
		corr:       "plan-x-img-1",
		input:      map[string]any{"prompt": "a lighthouse at dusk", "width": float64(1024), "steps": float64(4)},
		inputRefs:  []string{"/tmp/ref.png"},
		outDir:     "/tmp/art/plan-x",
		location:   "gpu",
		wakeJob:    "plan-x",
		parentJob:  "plan-root",
		vramMB:     12000,
		ramMB:      2048,
		deadline:   &dl,
	})
	if err != nil {
		t.Fatalf("buildSubmitMessage: %v", err)
	}
	if m.SchemaVersion != 2 {
		t.Errorf("v = %d, want 2", m.SchemaVersion)
	}
	if m.Capability != "image.generate" || m.ReplyTo != "flow.plan-x" ||
		m.CorrelationID != "plan-x-img-1" {
		t.Errorf("core v2 fields = %+v", m)
	}
	if m.Input["prompt"] != "a lighthouse at dusk" {
		t.Errorf("input.prompt = %v", m.Input["prompt"])
	}
	if m.Input["width"] != float64(1024) {
		t.Errorf("input.width = %#v, want number 1024", m.Input["width"])
	}
	if len(m.InputRefs) != 1 || m.InputRefs[0] != "/tmp/ref.png" {
		t.Errorf("input_refs = %v", m.InputRefs)
	}
	if m.OutDir != "/tmp/art/plan-x" || m.Location != "gpu" {
		t.Errorf("out_dir/location = %q / %q", m.OutDir, m.Location)
	}
	if m.WakeJob != "plan-x" || m.ParentJob != "plan-root" {
		t.Errorf("wake_job/parent_job = %q / %q", m.WakeJob, m.ParentJob)
	}
	if m.Resources == nil || m.Resources.VRAMMB != 12000 || m.Resources.RAMMB != 2048 {
		t.Errorf("resources = %+v, want vram 12000 ram 2048", m.Resources)
	}
	if m.Deadline == nil || !m.Deadline.Equal(dl) {
		t.Errorf("deadline = %v, want %v", m.Deadline, dl)
	}
	if m.ID != "test-submit-1" {
		t.Errorf("id = %q", m.ID)
	}
}

// TestBuildSubmitMessageNoHints checks the defaults: no --vram-mb/--ram-mb
// leaves Resources nil and no --deadline leaves Deadline nil, so the JSON
// omits both (the worker's defaults and the listener's timeout apply).
func TestBuildSubmitMessageNoHints(t *testing.T) {
	m, err := buildSubmitMessage(submitParams{
		capability: "audio.transcribe",
		id:         "test-submit-2",
		replyTo:    "flow.plan-y",
		corr:       "plan-y-a1",
	})
	if err != nil {
		t.Fatalf("buildSubmitMessage: %v", err)
	}
	if m.Resources != nil {
		t.Errorf("resources = %+v, want nil without hints", m.Resources)
	}
	if m.Deadline != nil {
		t.Errorf("deadline = %v, want nil", m.Deadline)
	}
	if m.Input != nil {
		t.Errorf("input = %v, want nil when no --input", m.Input)
	}
	if m.SchemaVersion != 2 {
		t.Errorf("v = %d, want 2 (a submit is always a v2 request)", m.SchemaVersion)
	}
}

// TestBuildSubmitMessageRequires is the contract a listener and a Result
// rely on: no reply queue, no correlation id, no request.
func TestBuildSubmitMessageRequires(t *testing.T) {
	base := submitParams{capability: "image.generate", id: "x", replyTo: "flow.j", corr: "c"}
	for name, mutate := range map[string]func(*submitParams){
		"no capability": func(p *submitParams) { p.capability = "" },
		"no reply-to":   func(p *submitParams) { p.replyTo = "" },
		"no corr":       func(p *submitParams) { p.corr = "" },
	} {
		p := base
		mutate(&p)
		if _, err := buildSubmitMessage(p); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

// TestInputFlagTyping pins how --input k=v values are typed: JSON parses to
// a number/bool/list, and anything else stays the plain string the user
// typed (a prompt with spaces, a ratio like 16:9).
func TestInputFlagTyping(t *testing.T) {
	f := inputFlag{}
	for _, v := range []string{
		"prompt=a lighthouse at dusk",
		"width=1024",
		"tile=true",
		"aspect=16:9",
		"refs=[\"a.png\", \"b.png\"]",
	} {
		if err := f.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
	if f["prompt"] != "a lighthouse at dusk" {
		t.Errorf("prompt = %#v", f["prompt"])
	}
	if f["width"] != float64(1024) {
		t.Errorf("width = %#v, want number", f["width"])
	}
	if f["tile"] != true {
		t.Errorf("tile = %#v, want bool", f["tile"])
	}
	if f["aspect"] != "16:9" {
		t.Errorf("aspect = %#v, want the string", f["aspect"])
	}
	refs, ok := f["refs"].([]any)
	if !ok || len(refs) != 2 {
		t.Errorf("refs = %#v, want a 2-element list", f["refs"])
	}
	if err := f.Set("noequals"); err == nil {
		t.Error("Set(\"noequals\"): expected an error")
	}
}

// TestParseDeadline accepts both forms: an absolute RFC3339 time and a
// duration from now.
func TestParseDeadline(t *testing.T) {
	tt, err := time.Parse(time.RFC3339, "2026-11-09T09:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseDeadline("2026-11-09T09:00:00Z")
	if err != nil {
		t.Fatalf("parseDeadline(rfc3339): %v", err)
	}
	if !got.Equal(tt) {
		t.Errorf("deadline = %v, want %v", got, tt)
	}
	before := time.Now().UTC()
	got, err = parseDeadline("2h")
	if err != nil {
		t.Fatalf("parseDeadline(2h): %v", err)
	}
	want := before.Add(2 * time.Hour)
	if got.Before(before.Add(110*time.Second)) || got.After(want.Add(10*time.Second)) {
		t.Errorf("deadline = %v, want about %v", got, want)
	}
	for _, bad := range []string{"tomorrow", "-5s", "12:30"} {
		if _, err := parseDeadline(bad); err == nil {
			t.Errorf("parseDeadline(%q): expected an error", bad)
		}
	}
}

// TestValidCapabilityName pins `submit`'s capability guard (EVB-17): a
// dotted path is the convention, but the cap.echo test worker is single-word
// and the Phase-2 gate submits to it, so a single word must pass. Malformed
// input (empty, spaces, uppercase, stray characters) is still rejected.
func TestValidCapabilityName(t *testing.T) {
	for _, ok := range []string{"echo", "image.generate", "audio.transcribe", "llm.small", "cap2.run"} {
		if !validCapabilityName(ok) {
			t.Errorf("validCapabilityName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "image. generate", "IMAGE.generate", "image.gen-erate", "image..generate"} {
		if validCapabilityName(bad) {
			t.Errorf("validCapabilityName(%q) = true, want false", bad)
		}
	}
}

// TestSubmitPublishesEnvelopeV2 is a live test (RabbitMQ on RABBITMQ_URL,
// default localhost:5672): `submit`'s message must arrive on cap.<cap>
// with the v2 JSON fields AND the mirrored AMQP properties (correlation_id,
// reply_to) — the property mirroring is what a Python worker or the
// broker's own logs see (plan §3.3). Skips when the broker is unreachable,
// so `go test ./...` stays green off this machine.
func TestSubmitPublishesEnvelopeV2(t *testing.T) {
	conn, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer conn.Close()

	capName := fmt.Sprintf("test.echo.%d", time.Now().UnixNano())
	qname := queue.CapQueueName(capName)
	t.Cleanup(func() {
		ch, err := conn.NewChannel()
		if err != nil {
			return
		}
		defer ch.Close()
		ch.QueueDelete(qname, true, false, false)
	})
	if err := conn.EnsureCapQueue(qname); err != nil {
		t.Fatalf("ensure cap queue: %v", err)
	}

	dl := time.Now().UTC().Add(time.Hour)
	m, err := buildSubmitMessage(submitParams{
		capability: capName,
		id:         "evb15-live",
		replyTo:    "flow.evb15-live",
		corr:       "evb15-corr-1",
		input:      map[string]any{"prompt": "echo test", "width": float64(64)},
		outDir:     "/tmp/evb15",
		deadline:   &dl,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := conn.Publish(m, qname+".default"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Consume it straight off the queue and check both the wire properties
	// and the JSON body.
	ch, err := conn.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	del, ok, err := ch.Get(qname, true)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok {
		t.Fatal("cap queue empty after publish")
	}
	if del.CorrelationId != "evb15-corr-1" {
		t.Errorf("AMQP correlation_id = %q, want evb15-corr-1", del.CorrelationId)
	}
	if del.ReplyTo != "flow.evb15-live" {
		t.Errorf("AMQP reply_to = %q, want flow.evb15-live", del.ReplyTo)
	}
	if del.ContentType != "application/json" {
		t.Errorf("content_type = %q, want application/json", del.ContentType)
	}
	got, err := queue.DecodeMessage(del.Body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Capability != capName || got.CorrelationID != "evb15-corr-1" ||
		got.ReplyTo != "flow.evb15-live" || got.SchemaVersion != 2 {
		t.Errorf("decoded v2 = capability %q corr %q reply-to %q v %d",
			got.Capability, got.CorrelationID, got.ReplyTo, got.SchemaVersion)
	}
	if got.Input["prompt"] != "echo test" || got.Input["width"] != float64(64) {
		t.Errorf("input = %v", got.Input)
	}
	if got.OutDir != "/tmp/evb15" || got.Deadline == nil || !got.Deadline.Equal(dl) {
		t.Errorf("out_dir/deadline = %q / %v", got.OutDir, got.Deadline)
	}
}

// TestValidateTell pins the tell kind/wake rule (plan §3.8, EVB-36): a
// wake is a listener-wake pair, and only review_request carries one — fyi/
// handoff are fire and forget, and a question's wake belongs to `ask`.
func TestValidateTell(t *testing.T) {
	if err := validateTell(queue.MailKindReviewRequest, "job-1"); err != nil {
		t.Errorf("review_request + wake = %v, want ok", err)
	}
	for _, kind := range []string{queue.MailKindFYI, queue.MailKindHandoff, queue.MailKindQuestion, ""} {
		if err := validateTell(kind, ""); err != nil {
			t.Errorf("%s without wake = %v, want ok", kind, err)
		}
	}
	for _, kind := range []string{queue.MailKindFYI, queue.MailKindHandoff, queue.MailKindQuestion, ""} {
		if err := validateTell(kind, "job-1"); err == nil {
			t.Errorf("%s + wake: want an error, got nil", kind)
		}
	}
}

// callRefusalDoc is a minimal catalog: one capability that runs
// alongside the brain, one that needs the swap window.
const callRefusalDoc = `
capabilities:
  echo:
    workers:
      - location: cpu
        model: echo
        vram_mb: 0
        ram_mb: 64
        needs_brain_out: false
        start: ""
        stop: ""
        typical_runtime: 30s
        idle_s: 0
  photo.generate:
    workers:
      - location: gpu
        model: zimage-turbo
        vram_mb: 12288
        ram_mb: 4096
        needs_brain_out: true
        start: "docker compose up -d art"
        stop: "docker compose stop art"
        typical_runtime: 45s
        idle_s: 300
`

// TestCallRefusesBrainOutBeforeTheBroker is the CLI half of the EVB-38
// refusal gate: a capability with a needs_brain_out placement must be
// refused with the deadlock message BEFORE any broker connection (the
// test sets no broker up at all — a dial attempt would fail or hang),
// and an alongside capability passes the gate (it then fails on the
// broker dial, a different error).
func TestCallRefusesBrainOutBeforeTheBroker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capabilities.yaml")
	if err := os.WriteFile(path, []byte(callRefusalDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JARVIS_CAPABILITIES", path)

	err := runCall([]string{"photo.generate", "--timeout", "1s"})
	if err == nil {
		t.Fatal("call photo.generate: expected the brain-out refusal, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to call photo.generate") ||
		!strings.Contains(err.Error(), "deadlock") ||
		!strings.Contains(err.Error(), "submit + yield") {
		t.Errorf("refusal = %q, want it to name the deadlock and the submit + yield alternative", err)
	}

	// The gate passes for an alongside capability: with the broker
	// pointed at a closed port the run dies at the dial — not with a
	// refusal. (A dead port fails the dial instantly, so the test stays
	// hermetic: it never touches the real broker or its queues.)
	t.Setenv("RABBITMQ_URL", "amqp://jarvis:jarvis-local@127.0.0.1:59999")
	if err := runCall([]string{"echo", "--timeout", "1s"}); err == nil || strings.Contains(err.Error(), "refusing to call") {
		t.Errorf("call echo: gate should pass and the run should die at the dial; got %v", err)
	}
}

// TestCallFlagValidation pins the argument errors that must fire before
// the catalog or the broker: no capability, an invalid name, a
// non-positive timeout.
func TestCallFlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--timeout", "1s"},
		{"Image.Generate"},
		{"echo", "--timeout", "0s"},
		{"echo", "extra"},
	} {
		if err := runCall(args); err == nil {
			t.Errorf("runCall(%v): expected an error, got nil", args)
		}
	}
}
