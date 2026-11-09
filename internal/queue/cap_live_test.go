package queue

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// mgmtQueue fetches a queue's info from the RabbitMQ management API: the
// AMQP QueueInspect reply carries neither durable nor arguments, so those
// assertions can only be made here. The default broker (connection.go) is
// the local compose rabbitmq, whose management port is bound to
// 127.0.0.1:15672 — if it is unreachable the test skips, like the rest of
// the live tests, so `go test ./...` stays green off this machine.
func mgmtQueue(t *testing.T, name string) map[string]any {
	t.Helper()
	u := "http://127.0.0.1:15672/api/queues/%2F/" + url.PathEscape(name)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.SetBasicAuth("jarvis", "jarvis-local")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("RabbitMQ management API unavailable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("management API %s: status %d", u, resp.StatusCode)
	}
	var info map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode queue info: %v", err)
	}
	return info
}

// TestEnsureCapQueueArgs is a live test (RabbitMQ on RABBITMQ_URL): a cap
// queue is durable with x-max-priority 10 and the dead-letter exchange
// (plan §3.4), the declaration is idempotent (a second EnsureCapQueue is a
// no-op), and a queue declared before the priority arg existed is migrated
// on the next empty pass — but a queue holding messages keeps its old
// shape (deleting it would lose its messages).
func TestEnsureCapQueueArgs(t *testing.T) {
	c, err := NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	name := CapQueueName(fmt.Sprintf("capargs-%d", time.Now().UnixNano()))
	defer deleteQueue(t, c, name)

	if err := c.EnsureCapQueue(name); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	info := mgmtQueue(t, name)
	if info["durable"] != true {
		t.Errorf("queue is not durable")
	}
	args, _ := info["arguments"].(map[string]any)
	if got, want := fmt.Sprint(args["x-max-priority"]), "10"; got != want {
		t.Errorf("x-max-priority = %s, want %s", got, want)
	}
	if got, want := args["x-dead-letter-exchange"], DLXExchange; got != want {
		t.Errorf("x-dead-letter-exchange = %v, want %v", got, want)
	}

	// Idempotent: re-declaring with identical args is a no-op.
	if err := c.EnsureCapQueue(name); err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	// A message on the queue protects the old shape from migration:
	// EnsureCapQueue must leave it (and the message) untouched.
	holder := "capargs-holder"
	defer deleteQueue(t, c, holder)
	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if _, err := ch.QueueDeclare(holder, true, false, false, false, dlxArgs()); err != nil {
		t.Fatalf("declare holder: %v", err)
	}
	if err := ch.Publish("", holder, false, false, amqp.Publishing{Body: []byte("keep me")}); err != nil {
		t.Fatalf("publish to holder: %v", err)
	}
	ch.Close()
	if err := c.EnsureCapQueue(holder); err != nil {
		t.Fatalf("ensure holder: %v", err)
	}
	hargs, _ := mgmtQueue(t, holder)["arguments"].(map[string]any)
	if _, ok := hargs["x-max-priority"]; ok {
		t.Errorf("queue holding a message was migrated anyway (x-max-priority now set)")
	}
}

// TestEnsureCapQueueMigratesEmptyQueue is a live test: a cap queue
// declared without the priority arg (the pre-EVB-17 shape) is migrated by
// the next EnsureCapQueue when it holds nothing — the new args are in
// place afterwards.
func TestEnsureCapQueueMigratesEmptyQueue(t *testing.T) {
	c, err := NewConn()
	if err != nil {
		t.Skipf("RabbitMQ unavailable: %v", err)
	}
	defer c.Close()

	name := CapQueueName(fmt.Sprintf("capmig-%d", time.Now().UnixNano()))
	defer deleteQueue(t, c, name)

	// Declare the old shape first: durable, dead-letter args, no priority.
	ch, err := c.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if _, err := ch.QueueDeclare(name, true, false, false, false, dlxArgs()); err != nil {
		t.Fatalf("declare old shape: %v", err)
	}
	ch.Close()

	if err := c.EnsureCapQueue(name); err != nil {
		t.Fatalf("ensure (migration): %v", err)
	}
	args, _ := mgmtQueue(t, name)["arguments"].(map[string]any)
	if got, want := fmt.Sprint(args["x-max-priority"]), "10"; got != want {
		t.Errorf("after migration x-max-priority = %s, want %s", got, want)
	}
}
