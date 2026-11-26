package queue

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// Broker-backed integration test for ConsumeLoopEvents (ELR-1, M1.3b).
// The broker setup helpers are copied from
// internal/switchboard/integration_test.go (TestMain, getBroker): the
// test runs against a broker it owns — RABBITMQ_URL when set, otherwise a
// throwaway rabbitmq container — and skips when no broker is available,
// so a plain `go test ./...` never needs one. One deliberate
// adaptation: when RABBITMQ_URL is unset and the default local broker
// (defaultAMQPURL) is already reachable, it is reused instead of
// spinning up a second container — this machine's live tests in this
// package already run against it, and TestMain here covers the whole
// package.

// broker is the broker the integration test runs against.
type broker struct {
	url       string
	container string // docker container id; "" for an existing broker
}

var (
	brokerVal *broker
	brokerErr error
)

// TestMain provisions the broker once for the whole package and tears
// down the throwaway container afterwards.
func TestMain(m *testing.M) {
	switch {
	case os.Getenv("RABBITMQ_URL") != "":
		brokerVal = &broker{url: os.Getenv("RABBITMQ_URL")}
	default:
		if c, err := NewConn(); err == nil {
			c.Close()
			brokerVal = &broker{url: defaultAMQPURL}
		} else {
			brokerVal, brokerErr = setupBroker()
		}
	}
	code := m.Run()
	if brokerVal != nil && brokerVal.container != "" {
		exec.Command("docker", "rm", "-f", brokerVal.container).Run()
	}
	os.Exit(code)
}

// getBroker returns the package broker, or skips the test when none is
// available (no RABBITMQ_URL, no default broker and no docker).
func getBroker(t *testing.T) *broker {
	t.Helper()
	if brokerErr != nil {
		t.Skipf("no broker for the integration test: %v", brokerErr)
	}
	return brokerVal
}

// setupBroker starts a throwaway rabbitmq on a random host loopback port
// and points RABBITMQ_URL at it (NewConn reads the env at connect time).
func setupBroker() (*broker, error) {
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
		conn, err := NewConn()
		if err == nil {
			conn.Close()
			return nil
		}
		last = err
		time.Sleep(1500 * time.Millisecond)
	}
	return fmt.Errorf("broker at %s not ready in %s: %w", url, timeout, last)
}

// connect dials the broker and closes it on test cleanup.
func connect(t *testing.T) *Conn {
	t.Helper()
	conn, err := NewConn()
	if err != nil {
		t.Fatalf("connect to %s: %v", os.Getenv("RABBITMQ_URL"), err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// waitFor polls cond every 50 ms until it is true or d elapses (then
// fails).
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

// TestConsumeLoopEvents: publish three events and consume them with a
// handler that fails the second one once. The handler must see all three
// events and the second one twice: the failed delivery is republished
// with the bumped x-attempts header (behind the third event) and
// redelivered, where it succeeds.
func TestConsumeLoopEvents(t *testing.T) {
	getBroker(t)
	conn := connect(t)

	evs := make([]LoopEvent, 3)
	for i := range evs {
		evs[i] = NewLoopEvent(EvOpDone, fmt.Sprintf("consume-loop-events-%d", i))
		if err := conn.PublishLoopEvent(context.Background(), evs[i]); err != nil {
			t.Fatalf("publish %s: %v", evs[i].ID, err)
		}
	}

	var (
		mu             sync.Mutex
		seen           = map[string]int{}
		failSecondOnce = false
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		handle := func(ev LoopEvent) error {
			mu.Lock()
			seen[ev.ID]++
			shouldFail := ev.ID == evs[1].ID && !failSecondOnce
			if shouldFail {
				failSecondOnce = true
			}
			mu.Unlock()
			if shouldFail {
				return errors.New("simulated handler failure")
			}
			return nil
		}
		done <- conn.ConsumeLoopEvents(ctx, handle)
	}()

	waitFor(t, 15*time.Second, "all three events consumed, the second twice", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return seen[evs[0].ID] == 1 && seen[evs[1].ID] == 2 && seen[evs[2].ID] == 1
	})
	mu.Lock()
	total := 0
	for _, n := range seen {
		total += n
	}
	mu.Unlock()
	if total != 4 {
		t.Fatalf("handler saw %d deliveries (%v), want 4 (three events, the second twice)", total, seen)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ConsumeLoopEvents after cancel: %v, want nil", err)
	}
}
