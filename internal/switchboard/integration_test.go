//go:build integration

// Broker-backed integration tests for the switchboard — the four acceptance
// criteria from plan task 14 (EVB-14):
//
//   1. a listener on a flow.* queue wakes its job within 5 s of the result;
//   2. chaos: kill the switchboard mid-wait AND restart the broker mid-wait,
//      and the job still wakes (durable listener + durable message +
//      resubscribe);
//   3. a listener whose correlation never arrives by its deadline wakes the
//      job with status=timeout;
//   4. the switchboard_* series are exposed at the /metrics endpoint.
//
// They run against a broker the test owns: a throwaway rabbitmq:3-management
// container on a random host loopback port (the default path, so it may be
// restarted for the chaos test), or an existing RABBITMQ_URL from the
// environment (an external broker the test will not disturb — the chaos
// broker-restart half skips in that case). With neither available the tests
// skip, so a plain `go test ./...` never needs a broker.
//
// Run with:  go test -tags integration ./internal/switchboard/

package switchboard

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// broker is the broker the integration tests run against. It is either the
// RABBITMQ_URL from the environment (an external broker the test must not
// disturb) or a throwaway rabbitmq container the test provisions and owns, so
// it may restart it in place for the chaos test.
type broker struct {
	url       string
	container string // docker container id; "" for an external broker
}

// restart stops and starts the throwaway container in place, so its durable
// queues and messages come back (that is exactly what the chaos test checks).
// For an external broker it skips: the test must not restart the production
// broker.
func (b *broker) restart(t *testing.T) {
	t.Helper()
	if b.container == "" {
		t.Skip("RABBITMQ_URL points at an external broker; the broker-restart half of the chaos test needs the throwaway container")
	}
	// -t 30 gives rabbitmq time to flush its mnesia store on a clean SIGTERM
	// shutdown, which is what preserves the durable queue and its message.
	if out, err := exec.Command("docker", "stop", "-t", "30", b.container).CombinedOutput(); err != nil {
		t.Fatalf("docker stop %s: %v: %s", b.container, err, out)
	}
	if out, err := exec.Command("docker", "start", b.container).CombinedOutput(); err != nil {
		t.Fatalf("docker start %s: %v: %s", b.container, err, out)
	}
	if err := waitBrokerReady(b.url, 90*time.Second); err != nil {
		t.Fatalf("broker not back after restart: %v", err)
	}
}

var (
	brokerVal *broker
	brokerErr error
)

// TestMain provisions the broker once for the whole package and tears down the
// throwaway container afterwards. It lives in this tagged file, so the
// broker-free unit tests (no tag) never run it.
func TestMain(m *testing.M) {
	brokerVal, brokerErr = setupBroker()
	code := m.Run()
	if brokerVal != nil && brokerVal.container != "" {
		exec.Command("docker", "rm", "-f", brokerVal.container).Run()
	}
	os.Exit(code)
}

// getBroker returns the package broker, or skips the test when none is
// available (no RABBITMQ_URL and no docker).
func getBroker(t *testing.T) *broker {
	t.Helper()
	if brokerErr != nil {
		t.Skipf("no broker for the integration test: %v", brokerErr)
	}
	return brokerVal
}

// setupBroker uses RABBITMQ_URL if it is set; otherwise it starts a throwaway
// rabbitmq on a random host loopback port and points RABBITMQ_URL at it
// (queue.NewConn reads the env at connect time).
func setupBroker() (*broker, error) {
	if url := os.Getenv("RABBITMQ_URL"); url != "" {
		return &broker{url: url}, nil
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker not found and RABBITMQ_URL is not set")
	}
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("free port: %w", err)
	}
	out, err := exec.Command("docker", "run", "-d",
		fmt.Sprintf("-p=127.0.0.1:%d:5672", port),
		"-e", "RABBITMQ_DEFAULT_USER=jarvis",
		"-e", "RABBITMQ_DEFAULT_PASS=jarvis-local",
		"rabbitmq:3-management").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run rabbitmq: %v: %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	b := &broker{url: fmt.Sprintf("amqp://jarvis:jarvis-local@127.0.0.1:%d", port), container: id}
	if err := waitBrokerReady(b.url, 120*time.Second); err != nil {
		exec.Command("docker", "rm", "-f", id).Run()
		return nil, err
	}
	return b, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitBrokerReady points RABBITMQ_URL at url and blocks until an AMQP
// connection succeeds (NewConn also declares the tasks exchange).
func waitBrokerReady(url string, timeout time.Duration) error {
	os.Setenv("RABBITMQ_URL", url)
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		conn, err := queue.NewConn()
		if err == nil {
			conn.Close()
			return nil
		}
		last = err
		time.Sleep(1500 * time.Millisecond)
	}
	return fmt.Errorf("broker at %s not ready in %s: %w", url, timeout, last)
}

// connect dials the broker (RABBITMQ_URL is set by setupBroker/waitBrokerReady)
// and closes it on test cleanup.
func connect(t *testing.T) *queue.Conn {
	t.Helper()
	conn, err := queue.NewConn()
	if err != nil {
		t.Fatalf("connect to %s: %v", os.Getenv("RABBITMQ_URL"), err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// publishResult publishes a result to a flow queue the way a worker publishes
// its reply: the queue is durable and the delivery is persistent, so it
// survives a broker restart.
func publishResult(t *testing.T, conn *queue.Conn, q string, r queue.Result) {
	t.Helper()
	if err := conn.EnsureFlowQueue(q); err != nil {
		t.Fatalf("ensure %s: %v", q, err)
	}
	ch, err := conn.NewChannel()
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	defer ch.Close()
	body, err := queue.EncodeResult(r)
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	if err := ch.PublishWithContext(context.Background(), "", q, false, false, amqp.Publishing{
		Body:          body,
		ContentType:   "application/json",
		CorrelationId: r.CorrelationID,
		DeliveryMode:  amqp.Persistent,
	}); err != nil {
		t.Fatalf("publish to %s: %v", q, err)
	}
}

// waitFor polls cond every 50 ms until it is true or d elapses (then fails).
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

// TestIntegrationWake is acceptance criterion 1: a listener on a flow queue
// wakes its job within 5 s of the result landing.
func TestIntegrationWake(t *testing.T) {
	getBroker(t)
	jobs := newJobs(t)
	ls, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open listeners: %v", err)
	}
	m, _ := NewMetrics()

	conn := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := New(Config{Conn: conn, Jobs: jobs, Listen: ls, M: m, PollInterval: 100 * time.Millisecond})
	go sb.Run(ctx)

	addRunning(t, jobs, "wake-1")
	yield(t, jobs, "wake-1", []string{"c-wake"}, time.Minute)
	if err := ls.Create(Listener{ID: "wake-1", Queue: "flow.wake-1", Action: ActionWake, JobID: "wake-1"}); err != nil {
		t.Fatalf("create listener: %v", err)
	}
	publishResult(t, conn, "flow.wake-1", queue.Result{CorrelationID: "c-wake", Status: "ok", Summary: "the result"})

	// The job must wake within 5 s of the result landing. The metric is part of
	// the condition (not just asserted after) because doWake sets the status
	// ready a hair before it bumps WakesTotal.
	waitFor(t, 5*time.Second, "the job to wake", func() bool {
		j, err := jobs.Get("wake-1")
		return err == nil && j.Status == agentjob.Ready && testutil.ToFloat64(m.WakesTotal) == 1
	})
}

// TestIntegrationChaos is acceptance criterion 2: kill the switchboard
// mid-wait and restart the broker mid-wait, and the job still wakes. The
// listener is durable (on disk, re-read on restart), the result is durable
// (survives the broker restart), and the switchboard resubscribes to the
// restarted broker.
func TestIntegrationChaos(t *testing.T) {
	b := getBroker(t)
	jobs := newJobs(t)
	ls, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open listeners: %v", err)
	}
	m, _ := NewMetrics()

	addRunning(t, jobs, "chaos-1")
	yield(t, jobs, "chaos-1", []string{"c-chaos"}, time.Minute)
	if err := ls.Create(Listener{ID: "chaos-1", Queue: "flow.chaos-1", Action: ActionWake, JobID: "chaos-1"}); err != nil {
		t.Fatalf("create listener: %v", err)
	}

	// Phase 1: the switchboard is up and waiting (no result yet).
	conn1 := connect(t)
	ctx1, cancel1 := context.WithCancel(context.Background())
	sb1 := New(Config{Conn: conn1, Jobs: jobs, Listen: ls, M: m, PollInterval: 100 * time.Millisecond})
	go sb1.Run(ctx1)
	time.Sleep(500 * time.Millisecond) // settle into the wait
	if j, _ := jobs.Get("chaos-1"); j.Status != agentjob.Waiting {
		t.Fatalf("job left the waiting state before the chaos: %s", j.Status)
	}
	cancel1() // kill the switchboard mid-wait
	conn1.Close()

	// Phase 2: publish the result, then restart the broker. The durable message
	// must come back with the broker.
	conn2 := connect(t)
	publishResult(t, conn2, "flow.chaos-1", queue.Result{CorrelationID: "c-chaos", Status: "ok", Summary: "chaos result"})
	conn2.Close()
	b.restart(t) // docker stop + start the throwaway broker

	// Phase 3: a fresh switchboard reconnects, re-reads the listener from disk,
	// peeks the restarted broker's queue, and wakes the job.
	conn3 := connect(t)
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	sb3 := New(Config{Conn: conn3, Jobs: jobs, Listen: ls, M: m, PollInterval: 100 * time.Millisecond})
	go sb3.Run(ctx3)
	waitFor(t, 5*time.Second, "the job to wake after the chaos", func() bool {
		j, err := jobs.Get("chaos-1")
		return err == nil && j.Status == agentjob.Ready
	})
}

// TestIntegrationTimeout is acceptance criterion 3: a listener whose
// correlation never arrives by its deadline wakes the job with
// status=timeout.
func TestIntegrationTimeout(t *testing.T) {
	getBroker(t)
	jobs := newJobs(t)
	ls, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open listeners: %v", err)
	}
	m, _ := NewMetrics()

	conn := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Short intervals so the deadline check fires soon after the (short) wait.
	sb := New(Config{Conn: conn, Jobs: jobs, Listen: ls, M: m, PollInterval: 100 * time.Millisecond, TimeoutInterval: 200 * time.Millisecond})
	go sb.Run(ctx)

	addRunning(t, jobs, "to-1")
	yield(t, jobs, "to-1", []string{"c-to"}, 1200*time.Millisecond) // ~1.2 s deadline
	if err := ls.Create(Listener{ID: "to-1", Queue: "flow.to-1", Action: ActionWake, JobID: "to-1"}); err != nil {
		t.Fatalf("create listener: %v", err)
	}
	// No result lands on flow.to-1, so only the timeout can wake it.

	waitFor(t, 6*time.Second, "the job to time out", func() bool {
		j, err := jobs.Get("to-1")
		return err == nil && j.Status == agentjob.Ready && testutil.ToFloat64(m.TimeoutsTotal) == 1
	})
	rec, err := jobs.ReadWake("to-1")
	if err != nil {
		t.Fatalf("read wake: %v", err)
	}
	if rec.Reason != agentjob.WakeTimeout {
		t.Fatalf("wake reason = %s, want timeout", rec.Reason)
	}
}

// TestIntegrationMetrics is acceptance criterion 4: the switchboard_* series
// are exposed at the /metrics endpoint (the private registry main.go mounts).
func TestIntegrationMetrics(t *testing.T) {
	getBroker(t)
	jobs := newJobs(t)
	ls, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open listeners: %v", err)
	}
	m, reg := NewMetrics()

	conn := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := New(Config{Conn: conn, Jobs: jobs, Listen: ls, M: m, PollInterval: 100 * time.Millisecond})
	go sb.Run(ctx)

	// One active listener gives the gauge a non-zero value to assert against.
	if err := ls.Create(Listener{ID: "m-1", Queue: "flow.m-1", Action: ActionWake, JobID: "m-1"}); err != nil {
		t.Fatalf("create listener: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // let a poll pass set the gauge

	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	for _, want := range []string{
		"switchboard_listeners_active",
		"switchboard_wakes_total",
		"switchboard_forwards_total",
		"switchboard_notifies_total",
		"switchboard_timeouts_total",
		"switchboard_resubscribes_total",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("/metrics is missing %s", want)
		}
	}
	if got := testutil.ToFloat64(m.ListenersActive); got != 1 {
		t.Fatalf("listeners_active = %v, want 1", got)
	}
}
