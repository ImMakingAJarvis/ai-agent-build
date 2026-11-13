package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/config"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// TestSubmitCapabilityAccessDenied covers the permission gate: a limited bot
// must not be able to spend owner money on a capability job.
func TestSubmitCapabilityAccessDenied(t *testing.T) {
	agent := New(Config{Model: "jarvis-brain", AccessLevel: config.AccessLimited})
	agent.Register(CapabilityAction())
	res := &Result{Type: TypeAction, ActionName: "submit_capability",
		Parameters: map[string]string{"capability": "echo"}}
	status, err := agent.Execute(context.Background(), res)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if status != "access denied: this action requires full access" {
		t.Fatalf("status = %q", status)
	}
}

// TestSubmitCapabilityValidation covers the fast-fail path (no broker
// contact): missing or malformed parameters are rejected before publish.
func TestSubmitCapabilityValidation(t *testing.T) {
	agent := New(Config{Model: "jarvis-brain", AccessLevel: config.AccessFull})
	agent.Register(CapabilityAction())
	_, err := agent.Execute(context.Background(), &Result{
		Type: TypeAction, ActionName: "submit_capability", Parameters: map[string]string{},
	})
	if err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatalf("missing capability: err=%v", err)
	}
	_, err = agent.Execute(context.Background(), &Result{
		Type: TypeAction, ActionName: "submit_capability",
		Parameters: map[string]string{"capability": "Image_Describe"},
	})
	if err == nil || !strings.Contains(err.Error(), "not a valid capability name") {
		t.Fatalf("bad capability: err=%v", err)
	}
	_, err = agent.Execute(context.Background(), &Result{
		Type: TypeAction, ActionName: "submit_capability",
		Parameters: map[string]string{"capability": "echo", "deadline": "soon"},
	})
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("bad deadline: err=%v", err)
	}
}

// TestClassifySubmitCapability covers the LLM classification path: a
// "describe the photos in folder X" message routes to submit_capability
// with the capability and folder as parameters. The message is 30+ words so
// the fast path (short statements → chat) leaves it to the LLM.
func TestClassifySubmitCapability(t *testing.T) {
	mock := &mockOllamaClient{
		responses: []string{actionResponseJSON("submit_capability", map[string]string{
			"capability": "image.describe",
			"path":       "/home/agent/.local/share/jarvis/artifacts/evb32-photos",
		})},
	}
	agent := New(Config{Model: "jarvis-brain", AccessLevel: config.AccessFull})
	agent.Register(CapabilityAction())
	agent.SetOllamaClient(mock)
	result, err := agent.Classify(context.Background(),
		"please go ahead and describe all of the photos that are sitting in the folder I am pointing you at right now because I need a one line caption for each picture and I want to see what is actually in them")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if result.Type != TypeAction || result.ActionName != "submit_capability" {
		t.Fatalf("result = %+v", result)
	}
	if result.Parameters["capability"] != "image.describe" || result.Parameters["path"] == "" {
		t.Fatalf("params = %v", result.Parameters)
	}
}

// TestFormatCapResultDescribe renders an image.describe Result with its
// index.json artifact: one line per image, caption collapsed to one line.
func TestFormatCapResultDescribe(t *testing.T) {
	dir := t.TempDir()
	index := filepath.Join(dir, "index.json")
	idx := map[string]any{
		"capability": "image.describe",
		"count":      2,
		"images": []map[string]any{
			{"file": "amber.png", "caption": "a   golden retriever\nwearing a hat", "tags": []string{"dog", "hat"}},
			{"file": "luna.png", "caption": "a grey cat on a windowsill", "tags": []string{}},
		},
	}
	b, _ := json.Marshal(idx)
	if err := os.WriteFile(index, b, 0o644); err != nil {
		t.Fatal(err)
	}
	r := queue.Result{
		Capability:    "image.describe",
		CorrelationID: "hm-test",
		Status:        queue.ResultOK,
		Summary:       "described 2 images",
		Artifacts:     []string{filepath.Join(dir, "amber.json"), index},
		Worker:        "image-describe-worker",
	}
	out := FormatCapResult(r)
	for _, want := range []string{
		"**image.describe** — done (corr hm-test)",
		"described 2 images",
		"- amber.png: a golden retriever wearing a hat _(tags: dog hat)_",
		"- luna.png: a grey cat on a windowsill",
		"_worker: image-describe-worker_",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestFormatCapResultError(t *testing.T) {
	r := queue.Result{Capability: "echo", CorrelationID: "c1", Status: queue.ResultError, Error: "boom"}
	out := FormatCapResult(r)
	if !strings.Contains(out, "**echo** — failed (corr c1)") || !strings.Contains(out, "failed: boom") {
		t.Fatalf("out = %q", out)
	}
}

// TestWatchCapResultsLive covers the durable consumer: a non-result message
// is acked and skipped, a real Result is decoded and handed to onResult.
func TestWatchCapResultsLive(t *testing.T) {
	c, err := queue.NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()
	// A throwaway queue, never CapReplyQueue, so a live run cannot answer
	// with a spurious Discord reply.
	q := "flow.hermes-test-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := c.EnsureFlowQueue(q); err != nil {
		t.Fatalf("EnsureFlowQueue: %v", err)
	}
	got := make(chan queue.Result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- watchCapResultsOn(ctx, c, q, func(r queue.Result) { got <- r }, nil)
	}()
	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("NewChannel: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // let the consumer attach
	// A stray non-result message is skipped, not delivered.
	if err := ch.Publish("", q, false, false, amqp.Publishing{Body: []byte(`{"not":"a result"}`), DeliveryMode: amqp.Persistent}); err != nil {
		t.Fatalf("publish stray: %v", err)
	}
	body, err := queue.EncodeResult(queue.Result{
		CorrelationID: "hm-live", Capability: "echo", Status: queue.ResultOK, Summary: "hello",
	})
	if err != nil {
		t.Fatalf("EncodeResult: %v", err)
	}
	if err := ch.Publish("", q, false, false, amqp.Publishing{Body: body, DeliveryMode: amqp.Persistent}); err != nil {
		t.Fatalf("publish result: %v", err)
	}
	select {
	case r := <-got:
		if r.CorrelationID != "hm-live" || r.Status != queue.ResultOK {
			t.Fatalf("result = %+v", r)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no result delivered within 15s")
	}
	cancel()
	select {
	case <-done: // the watch loop returned on ctx cancel
	case <-time.After(5 * time.Second):
		t.Fatal("watch loop did not return on cancel")
	}
}
