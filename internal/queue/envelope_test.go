package queue

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// v2TestMessage builds a Message with every v2 field populated, so the
// round-trip test exercises the whole envelope.
func v2TestMessage(t *testing.T) Message {
	t.Helper()
	deadline := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	return Message{
		ID:        "evb8-img-1",
		Type:      "image",
		Priority:  5,
		Payload:   map[string]string{"prompt": "a blue square"},
		CreatedAt: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC),
		Source:    "cli",
		Capability:    "image.generate",
		CorrelationID: "evb8-img-1",
		ReplyTo:       "flow.chat-123",
		WakeJob:       "chat-123",
		ParentJob:     "chat-123",
		Input:         map[string]any{"prompt": "a blue square", "size": float64(512)},
		InputRefs:     []string{"/tmp/ref.png"},
		OutDir:        "/home/agent/.local/share/jarvis/artifacts/chat-123",
		Location:      "gpu",
		Resources:     &Resources{VRAMMB: 12000, RAMMB: 2048},
		Deadline:      &deadline,
		Attempt:       1,
		SchemaVersion: 2,
	}
}

// TestV2RoundTrip is the Phase 1 envelope acceptance: a fully-populated v2
// Message survives an encode/decode cycle with every field intact.
func TestV2RoundTrip(t *testing.T) {
	m := v2TestMessage(t)
	body, err := encodeMessage(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeMessage(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// v1 fields
	if got.ID != m.ID || got.Type != m.Type || got.Priority != m.Priority || got.Source != m.Source {
		t.Errorf("v1 fields = %+v", got)
	}
	if !reflect.DeepEqual(got.Payload, m.Payload) {
		t.Errorf("Payload = %v, want %v", got.Payload, m.Payload)
	}
	if !got.CreatedAt.Equal(m.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, m.CreatedAt)
	}
	// v2 fields
	if got.Capability != m.Capability {
		t.Errorf("Capability = %q, want %q", got.Capability, m.Capability)
	}
	if got.CorrelationID != m.CorrelationID {
		t.Errorf("CorrelationID = %q, want %q", got.CorrelationID, m.CorrelationID)
	}
	if got.ReplyTo != m.ReplyTo {
		t.Errorf("ReplyTo = %q, want %q", got.ReplyTo, m.ReplyTo)
	}
	if got.WakeJob != m.WakeJob {
		t.Errorf("WakeJob = %q, want %q", got.WakeJob, m.WakeJob)
	}
	if got.ParentJob != m.ParentJob {
		t.Errorf("ParentJob = %q, want %q", got.ParentJob, m.ParentJob)
	}
	if !reflect.DeepEqual(got.Input, m.Input) {
		t.Errorf("Input = %v, want %v", got.Input, m.Input)
	}
	if !reflect.DeepEqual(got.InputRefs, m.InputRefs) {
		t.Errorf("InputRefs = %v, want %v", got.InputRefs, m.InputRefs)
	}
	if got.OutDir != m.OutDir {
		t.Errorf("OutDir = %q, want %q", got.OutDir, m.OutDir)
	}
	if got.Location != m.Location {
		t.Errorf("Location = %q, want %q", got.Location, m.Location)
	}
	if got.Resources == nil || m.Resources == nil {
		t.Fatalf("Resources pointer = %v / %v, want both non-nil", got.Resources, m.Resources)
	}
	if *got.Resources != *m.Resources {
		t.Errorf("Resources = %+v, want %+v", *got.Resources, *m.Resources)
	}
	if got.Deadline == nil || !got.Deadline.Equal(*m.Deadline) {
		t.Errorf("Deadline = %v, want %v", got.Deadline, *m.Deadline)
	}
	if got.Attempt != m.Attempt {
		t.Errorf("Attempt = %d, want %d", got.Attempt, m.Attempt)
	}
	if got.SchemaVersion != m.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, m.SchemaVersion)
	}
}

// TestV1DecodeUnchanged is the backward-compat acceptance: a v1 message —
// with no v2 fields present — still decodes, and its v2 fields come back
// empty/zero. Both a minimal v1 body and one carrying the optional v1
// fields are covered.
func TestV1DecodeUnchanged(t *testing.T) {
	// Minimal v1: the original core fields only.
	minimal := `{"id":"v1-1","type":"coding","priority":1,` +
		`"payload":{"task":"fix the lint"},"created_at":"2026-11-01T08:00:00Z","source":"cli"}`
	m, err := DecodeMessage([]byte(minimal))
	if err != nil {
		t.Fatalf("decode minimal v1: %v", err)
	}
	if m.ID != "v1-1" || m.Type != "coding" || m.Priority != 1 || m.Source != "cli" {
		t.Errorf("v1 core = %+v", m)
	}
	assertV2Empty(t, m)

	// v1 with the optional v1 fields set: they decode, v2 stays empty.
	rich := `{"id":"v1-2","type":"coding","priority":2,"payload":{},` +
		`"created_at":"2026-11-01T08:00:00Z","source":"github","force_api":true,` +
		`"model_override":"sonnet","kind":"agent","title":"build thing",` +
		`"instructions":"do it","branch":"phase-x","depends_on":["a","b"],` +
		`"driver":"local","class":"fix","reasoning_effort":"high"}`
	r, err := DecodeMessage([]byte(rich))
	if err != nil {
		t.Fatalf("decode rich v1: %v", err)
	}
	if r.Kind != KindAgent || r.Title != "build thing" || r.Driver != "local" ||
		r.Class != "fix" || r.ReasoningEffort != EffortHigh || !r.ForceAPI ||
		r.ModelOverride != "sonnet" || len(r.DependsOn) != 2 {
		t.Errorf("v1 optional fields = %+v", r)
	}
	assertV2Empty(t, r)
}

// assertV2Empty checks every v2 field is at its zero value.
func assertV2Empty(t *testing.T, m Message) {
	t.Helper()
	if m.Capability != "" || m.CorrelationID != "" || m.ReplyTo != "" || m.WakeJob != "" ||
		m.ParentJob != "" || m.OutDir != "" || m.Location != "" {
		t.Errorf("v2 string fields not empty: %+v", m)
	}
	if m.Input != nil || m.InputRefs != nil || m.Resources != nil || m.Deadline != nil {
		t.Errorf("v2 pointer/slice/map fields not nil: Input=%v InputRefs=%v Resources=%v Deadline=%v",
			m.Input, m.InputRefs, m.Resources, m.Deadline)
	}
	if m.Attempt != 0 || m.SchemaVersion != 0 {
		t.Errorf("v2 int fields not zero: Attempt=%d SchemaVersion=%d", m.Attempt, m.SchemaVersion)
	}
}

// TestV2FieldsOmittedWhenEmpty: a v1 message (no v2 fields set) encodes to
// JSON that contains none of the v2 keys — omitempty keeps the wire format
// identical to what v1 consumers expect.
func TestV2FieldsOmittedWhenEmpty(t *testing.T) {
	v1 := Message{
		ID: "v1-3", Type: "voice", Priority: 1,
		Payload: map[string]string{"text": "hi"}, CreatedAt: time.Now().UTC(), Source: "discord",
	}
	body, err := encodeMessage(v1)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(body)
	for _, key := range []string{
		`"capability"`, `"correlation_id"`, `"reply_to"`, `"wake_job"`, `"parent_job"`,
		`"input"`, `"input_refs"`, `"out_dir"`, `"location"`, `"resources"`,
		`"deadline"`, `"attempt"`, `"v"`,
	} {
		if strings.Contains(s, key) {
			t.Errorf("v1-encoded JSON contains v2 key %s: %s", key, s)
		}
	}
}

// TestAMQPPropertyMirroring is the "set AMQP correlation_id/reply_to from the
// JSON fields" acceptance: the wire envelope carries the same values the JSON
// does, and an unset field leaves the AMQP property empty.
func TestAMQPPropertyMirroring(t *testing.T) {
	// Set: both mirror onto the AMQP properties.
	m := Message{
		ID: "evb8-m", Type: "image", CreatedAt: time.Now().UTC(),
		CorrelationID: "corr-1", ReplyTo: "flow.job-9",
	}
	pub, err := newPublishing(m, 7)
	if err != nil {
		t.Fatalf("newPublishing: %v", err)
	}
	if pub.CorrelationId != "corr-1" {
		t.Errorf("AMQP CorrelationId = %q, want %q", pub.CorrelationId, "corr-1")
	}
	if pub.ReplyTo != "flow.job-9" {
		t.Errorf("AMQP ReplyTo = %q, want %q", pub.ReplyTo, "flow.job-9")
	}
	if pub.Priority != 7 {
		t.Errorf("AMQP Priority = %d, want 7", pub.Priority)
	}
	if pub.ContentType != "application/json" || pub.DeliveryMode != 2 {
		t.Errorf("envelope = %q/%d, want application/json/persistent", pub.ContentType, pub.DeliveryMode)
	}
	// The JSON body carries the same values.
	var decoded Message
	if err := json.Unmarshal(pub.Body, &decoded); err != nil {
		t.Fatalf("body does not decode: %v", err)
	}
	if decoded.CorrelationID != pub.CorrelationId || decoded.ReplyTo != pub.ReplyTo {
		t.Errorf("JSON %q/%q != AMQP %q/%q", decoded.CorrelationID, decoded.ReplyTo, pub.CorrelationId, pub.ReplyTo)
	}

	// Unset: empty v2 fields leave the AMQP properties empty.
	v1 := Message{ID: "v1-4", Type: "voice", CreatedAt: time.Now().UTC()}
	pub0, err := newPublishing(v1, 0)
	if err != nil {
		t.Fatalf("newPublishing(v1): %v", err)
	}
	if pub0.CorrelationId != "" || pub0.ReplyTo != "" {
		t.Errorf("empty message mirrors non-empty AMQP props: %q/%q", pub0.CorrelationId, pub0.ReplyTo)
	}
}

// TestResultRoundTrip: a Result survives an encode/decode cycle.
func TestResultRoundTrip(t *testing.T) {
	started := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	finished := time.Date(2026, 11, 8, 12, 1, 30, 0, time.UTC)
	r := Result{
		CorrelationID: "evb8-img-1",
		Capability:    "image.generate",
		Status:        ResultOK,
		Artifacts:     []string{"/artifacts/chat-123/square.png"},
		Summary:       "made the blue square",
		Data:          map[string]any{"width": float64(512), "height": float64(512)},
		Worker:        "image.generate@gpu/jarvis-art",
		Model:         "Z-Image-Turbo",
		StartedAt:     started,
		FinishedAt:    finished,
	}
	body, err := EncodeResult(r)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeResult(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CorrelationID != r.CorrelationID || got.Capability != r.Capability || got.Status != r.Status {
		t.Errorf("identity fields = %+v", got)
	}
	if !reflect.DeepEqual(got.Artifacts, r.Artifacts) {
		t.Errorf("Artifacts = %v, want %v", got.Artifacts, r.Artifacts)
	}
	if got.Summary != r.Summary {
		t.Errorf("Summary = %q, want %q", got.Summary, r.Summary)
	}
	if !reflect.DeepEqual(got.Data, r.Data) {
		t.Errorf("Data = %v, want %v", got.Data, r.Data)
	}
	if got.Worker != r.Worker || got.Model != r.Model {
		t.Errorf("Worker/Model = %q/%q, want %q/%q", got.Worker, got.Model, r.Worker, r.Model)
	}
	if !got.StartedAt.Equal(r.StartedAt) || !got.FinishedAt.Equal(r.FinishedAt) {
		t.Errorf("times = %v/%v, want %v/%v", got.StartedAt, got.FinishedAt, r.StartedAt, r.FinishedAt)
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty", got.Error)
	}
}

// TestResultOmitsErrorAndData: an ok Result with no Data and no Error encodes
// without those keys (omitempty), so a happy-path reply stays small.
func TestResultOmitsErrorAndData(t *testing.T) {
	body, err := EncodeResult(Result{CorrelationID: "c", Capability: "x", Status: ResultOK, Artifacts: []string{"/a.png"}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(body)
	if strings.Contains(s, `"error"`) || strings.Contains(s, `"data"`) {
		t.Errorf("ok Result should omit error/data keys: %s", s)
	}
}

// TestDoneMarker pins the idempotency marker path convention.
func TestDoneMarker(t *testing.T) {
	if got := DoneMarker("/artifacts/j", "c-1"); got != "/artifacts/j/.done-c-1" {
		t.Errorf("DoneMarker = %q, want /artifacts/j/.done-c-1", got)
	}
}

// TestV2AMQPPropertiesLive publishes a v2 Message through the real broker and
// checks that the native AMQP properties (correlation_id / reply_to) arrive
// alongside the JSON body — the end-to-end form of TestAMQPPropertyMirroring.
// It skips when RabbitMQ is unreachable.
func TestV2AMQPPropertiesLive(t *testing.T) {
	conn := liveConn(t)
	name := fmt.Sprintf("evb8.cap.%d", time.Now().UnixNano())
	t.Cleanup(func() { deleteQueue(t, conn, name) })

	ch, err := conn.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	if _, err := ch.QueueDeclare(name, true, false, false, false, nil); err != nil {
		t.Fatalf("declare scratch queue: %v", err)
	}
	if err := ch.QueueBind(name, name, conn.Exchange(), true, nil); err != nil {
		t.Fatalf("bind scratch queue: %v", err)
	}
	dels, err := ch.Consume(name, "", true, false, false, false, nil) // autoAck
	if err != nil {
		t.Fatalf("consume: %v", err)
	}

	m := Message{
		ID: "evb8-live", Type: "image",
		CreatedAt: time.Now().UTC(),
		Capability: "image.generate", CorrelationID: "evb8-corr",
		ReplyTo: "flow.evb8-live", SchemaVersion: 2,
	}
	if err := conn.Publish(m, name); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case d := <-dels:
		if d.CorrelationId != m.CorrelationID {
			t.Errorf("AMQP correlation_id = %q, want %q", d.CorrelationId, m.CorrelationID)
		}
		if d.ReplyTo != m.ReplyTo {
			t.Errorf("AMQP reply_to = %q, want %q", d.ReplyTo, m.ReplyTo)
		}
		var got Message
		if err := json.Unmarshal(d.Body, &got); err != nil {
			t.Fatalf("delivery does not decode: %v", err)
		}
		if got.CorrelationID != m.CorrelationID || got.ReplyTo != m.ReplyTo ||
			got.Capability != m.Capability || got.SchemaVersion != 2 {
			t.Errorf("decoded v2 = %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the delivery")
	}
}
