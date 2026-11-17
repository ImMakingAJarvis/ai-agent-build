package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"
)

// TestRemoteUserPermissionsSuffice is a live test (RabbitMQ on
// RABBITMQ_URL, default localhost:5672): rabbitmq/remote-user.sh must
// provision a least-privilege user whose grants cover the remote
// capworker's whole declaration and reply path — the shared dead
// topology, its own cap queue (declare, bind, consume), the retry
// republish through the tasks exchange, and the Result reply through
// the default exchange. The last one regressed once as "write access
// to exchange 'amq.default' refused" (a basic.publish on the default
// exchange checks write on the exchange itself, not just on the
// target queue), and the flow-queue re-declare regressed as "read
// access to queue 'flow.*' refused" (creating a queue checks read;
// re-declaring an existing one does not, which is why a probe against
// a pre-existing queue passes while a fresh node's first reply does
// not). It also pins the negative side: the user must not consume the
// dead queue. Skips when the broker is unreachable or the script
// cannot manage it (neither rabbitmqctl nor docker). A crashed run
// may leave a remote-permtest-* user behind — rabbitmqctl delete_user.
func TestRemoteUserPermissionsSuffice(t *testing.T) {
	nano := time.Now().UnixNano()
	node := fmt.Sprintf("permtest-%d", nano)
	capname := fmt.Sprintf("permtest%d", nano) // single word: valid for the script and ValidCapabilityName

	pwb := make([]byte, 12)
	if _, err := rand.Read(pwb); err != nil {
		t.Fatalf("generate password: %v", err)
	}
	pw := hex.EncodeToString(pwb)

	cmd := exec.Command("../../rabbitmq/remote-user.sh", node, capname)
	cmd.Env = append(os.Environ(), "REMOTE_USER_PASSWORD="+pw)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("remote-user.sh could not provision a user (needs rabbitmqctl or docker here): %v: %s", err, out)
	}

	u, err := url.Parse(AMQPURL())
	if err != nil {
		t.Fatalf("bad RABBITMQ_URL %q: %v", AMQPURL(), err)
	}
	u.User = url.UserPassword("remote-"+node, pw)
	rc, err := NewConnOpts(DialOpts{URL: u.String()})
	if err != nil {
		t.Skipf("broker unreachable as the provisioned user: %v", err)
	}
	capQ := CapQueueName(capname)
	flowName := FlowQueueName(fmt.Sprintf("permtest-%d", nano))
	defer deleteRemoteUser(t, "remote-"+node)
	defer rc.Close()
	defer rc.deleteBoth(t, flowName, capQ)

	ctx := context.Background()

	// startup: re-declare the shared dead topology
	if err := rc.EnsureDLX(); err != nil {
		t.Fatalf("EnsureDLX (worker startup, dead topology): %v", err)
	}
	// startup: declare + bind its own cap queue
	if err := rc.EnsureCapQueue(capQ); err != nil {
		t.Fatalf("EnsureCapQueue (worker startup): %v", err)
	}
	// worker loop: consume its cap queue (read on cap.<cap>)
	chc, err := rc.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	tag := fmt.Sprintf("permtest-%d", nano)
	if _, err := chc.Consume(capQ, tag, false, false, false, false, nil); err != nil {
		t.Fatalf("consume its cap queue (read): %v", err)
	}
	if err := chc.Cancel(tag, false); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	chc.Close()

	// retry republish: through the tasks exchange (write on tasks)
	ch, err := rc.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	if err := ch.Confirm(false); err != nil {
		t.Fatalf("confirm mode: %v", err)
	}
	confirms := make(chan amqp091.Confirmation, 1)
	ch.NotifyPublish(confirms)
	if err := ch.PublishWithContext(ctx, rc.Exchange(), capQ+".default", false, false, amqp091.Publishing{
		Body:         []byte(`{"probe":"permtest"}`),
		DeliveryMode: amqp091.Persistent,
	}); err != nil {
		t.Fatalf("retry republish through tasks: %v", err)
	}
	waitResultConfirm(t, confirms, "retry republish")

	// reply path: re-declare the flow queue — it does not exist yet,
	// so this checks read on flow.* (fresh-node first reply)
	if err := rc.EnsureFlowQueue(flowName); err != nil {
		t.Fatalf("EnsureFlowQueue (reply path, fresh queue): %v", err)
	}
	// reply path: the Result, through the default exchange — the
	// "write access to exchange 'amq.default' refused" regression
	if err := ch.PublishWithContext(ctx, "", flowName, false, false, amqp091.Publishing{
		Body:         []byte(`{"probe":"permtest"}`),
		ContentType:  "application/json",
		DeliveryMode: amqp091.Persistent,
		Timestamp:    time.Now(),
	}); err != nil {
		t.Fatalf("result publish through the default exchange: %v", err)
	}
	waitResultConfirm(t, confirms, "result publish")

	// negative: the user must not read/consume the dead queue
	neg, err := rc.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer neg.Close()
	negErr := make(chan error, 1)
	go func() {
		_, e := neg.Consume(DeadQueue, tag+"-neg", true, false, false, false, nil)
		negErr <- e
	}()
	select {
	case e := <-negErr:
		if e == nil {
			t.Fatal("consume on the dead queue succeeded — the worker must not read dead")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("consume on the dead queue hung (want an access-refused error)")
	}
}

// waitResultConfirm blocks until the broker confirms the publish.
func waitResultConfirm(t *testing.T, confirms chan amqp091.Confirmation, what string) {
	t.Helper()
	select {
	case cf := <-confirms:
		if !cf.Ack {
			t.Fatalf("broker nacked the %s", what)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for the %s confirm", what)
	}
}

// deleteRemoteUser removes a provisioned test user, using the same
// rabbitmqctl-or-docker detection rabbitmq/remote-user.sh uses.
func deleteRemoteUser(t *testing.T, user string) {
	t.Helper()
	var cmd *exec.Cmd
	if _, err := exec.LookPath("rabbitmqctl"); err == nil {
		cmd = exec.Command("rabbitmqctl", "delete_user", "-p", "/", user)
	} else {
		cmd = exec.Command("docker", "exec", "jarvis-rabbitmq", "rabbitmqctl", "delete_user", "-p", "/", user)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("delete_user %s: %v: %s", user, err, out)
	}
}
