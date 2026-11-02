package queue

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestLocationQueueFor(t *testing.T) {
	for loc, want := range map[string]string{
		"gpu":  JobsGPUQueue,
		"cpu":  JobsCPUQueue,
		"igpu": JobsIGPUQueue,
	} {
		got, ok := LocationQueueFor(loc)
		if !ok {
			t.Errorf("LocationQueueFor(%q): ok = false, want %q", loc, want)
			continue
		}
		if got != want {
			t.Errorf("LocationQueueFor(%q) = %q, want %q", loc, got, want)
		}
	}
	// Cloud has no box queue; anything else unknown is rejected.
	for _, loc := range []string{"cloud", "none", "", "gpux"} {
		if got, ok := LocationQueueFor(loc); ok {
			t.Errorf("LocationQueueFor(%q) = %q, ok = true, want not ok", loc, got)
		}
	}
}

func TestLocationWorkers_PauseRoutes(t *testing.T) {
	lw := NewLocationWorkers(nil, nil)
	lw.Pause(JobsGPUQueue)
	if !lw.IsPaused(JobsGPUQueue) {
		t.Error("IsPaused(jobs.gpu) = false after Pause, want true")
	}
	if lw.IsPaused(JobsCPUQueue) || lw.IsPaused(JobsIGPUQueue) {
		t.Error("pausing the GPU queue must not pause the cpu/igpu queues")
	}
	lw.Resume(JobsGPUQueue)
	if lw.IsPaused(JobsGPUQueue) {
		t.Error("IsPaused(jobs.gpu) = true after Resume, want false")
	}
	// An unknown queue is a no-op, not a panic.
	lw.Pause("jobs.nope")
	lw.Resume("jobs.nope")
	if lw.IsPaused("jobs.nope") {
		t.Error("IsPaused(unknown queue) = true, want false")
	}
}

// liveConn returns a connection to the local broker, or skips the test
// when RabbitMQ is not reachable.
func liveConn(t *testing.T) *Conn {
	t.Helper()
	conn, err := NewConn()
	if err != nil {
		t.Skipf("RabbitMQ not reachable: %v", err)
	}
	return conn
}

// deleteQueue drops a test queue on a throwaway channel.
func deleteQueue(t *testing.T, conn *Conn, name string) {
	t.Helper()
	ch, err := conn.NewChannel()
	if err != nil {
		return
	}
	defer ch.Close()
	_, _ = ch.QueueDelete(name, false, false, false)
}

// waitWG waits for wg with a deadline, failing the test on timeout.
func waitWG(t *testing.T, wg *sync.WaitGroup, d time.Duration, what string) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestLocationWorkers_CPUFinishesWhileGPURunning is the A6 acceptance
// check: a CPU job finishes while a long GPU job is still running. The
// test queues carry a per-run suffix under an a6test. prefix so their
// routing keys never match the real jobs.* bindings — the workers, the
// "tasks" exchange routing and the per-queue channels are the real ones.
func TestLocationWorkers_CPUFinishesWhileGPURunning(t *testing.T) {
	conn := liveConn(t)
	defer conn.Close()

	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	gpuQ := "a6test.gpu." + sfx
	cpuQ := "a6test.cpu." + sfx
	defer deleteQueue(t, conn, gpuQ)
	defer deleteQueue(t, conn, cpuQ)

	const (
		gpuRun = 2 * time.Second // the long GPU job
		cpuRun = 100 * time.Millisecond
	)

	var mu sync.Mutex
	times := map[string]time.Time{}
	record := func(k string, at time.Time) {
		mu.Lock()
		times[k] = at
		mu.Unlock()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	handler := func(m Message) error {
		loc := m.Payload["run"] // "gpu" or "cpu"
		if loc == "gpu" {
			record("gpuStart", time.Now())
			time.Sleep(gpuRun)
			record("gpuEnd", time.Now())
		} else {
			time.Sleep(cpuRun)
			record("cpuEnd", time.Now())
		}
		wg.Done()
		return nil
	}

	lw := NewLocationWorkers(conn, handler, gpuQ, cpuQ)
	if err := lw.Start(); err != nil {
		t.Fatalf("start location workers: %v", err)
	}
	defer lw.Stop()

	if err := conn.Publish(Message{
		ID: "a6-gpu-" + sfx, Type: "background", Source: "test",
		Payload: map[string]string{"run": "gpu"},
	}, gpuQ); err != nil {
		t.Fatalf("publish gpu job: %v", err)
	}
	if err := conn.Publish(Message{
		ID: "a6-cpu-" + sfx, Type: "background", Source: "test",
		Payload: map[string]string{"run": "cpu"},
	}, cpuQ); err != nil {
		t.Fatalf("publish cpu job: %v", err)
	}

	waitWG(t, &wg, 15*time.Second, "both jobs to finish")

	mu.Lock()
	defer mu.Unlock()
	cpuEnd, ok1 := times["cpuEnd"]
	gpuStart, ok2 := times["gpuStart"]
	gpuEnd, ok3 := times["gpuEnd"]
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("missing completion records: %v", times)
	}
	if !cpuEnd.After(gpuStart) {
		t.Errorf("CPU job finished at %s before the GPU job started (%s) — no overlap", cpuEnd, gpuStart)
	}
	if !cpuEnd.Before(gpuEnd) {
		t.Errorf("CPU job finished at %s after the GPU job finished (%s) — the CPU job waited behind the GPU job", cpuEnd, gpuEnd)
	}
}

// TestLocationWorkers_PausedGPUQueueDoesNotBlockCPU is the watchdog side
// of A6: while the GPU queue is paused (a game holds the GPU) CPU jobs
// keep running, the paused GPU job waits unacked, and runs after resume.
func TestLocationWorkers_PausedGPUQueueDoesNotBlockCPU(t *testing.T) {
	conn := liveConn(t)
	defer conn.Close()

	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	gpuQ := "a6test.gpu." + sfx
	cpuQ := "a6test.cpu." + sfx
	defer deleteQueue(t, conn, gpuQ)
	defer deleteQueue(t, conn, cpuQ)

	var mu sync.Mutex
	ran := map[string]bool{}
	mark := func(loc string) {
		mu.Lock()
		ran[loc] = true
		mu.Unlock()
	}
	saw := func(loc string) bool {
		mu.Lock()
		defer mu.Unlock()
		return ran[loc]
	}

	var wg sync.WaitGroup
	handler := func(m Message) error {
		time.Sleep(300 * time.Millisecond)
		mark(m.Payload["run"])
		wg.Done()
		return nil
	}

	lw := NewLocationWorkers(conn, handler, gpuQ, cpuQ)
	if err := lw.Start(); err != nil {
		t.Fatalf("start location workers: %v", err)
	}
	defer lw.Stop()

	// Game starts: the watchdog pauses only the GPU queue.
	lw.Pause(gpuQ)

	wg.Add(1)
	if err := conn.Publish(Message{
		ID: "a6-gpu-" + sfx, Type: "background", Source: "test",
		Payload: map[string]string{"run": "gpu"},
	}, gpuQ); err != nil {
		t.Fatalf("publish gpu job: %v", err)
	}
	wg.Add(1)
	if err := conn.Publish(Message{
		ID: "a6-cpu-" + sfx, Type: "background", Source: "test",
		Payload: map[string]string{"run": "cpu"},
	}, cpuQ); err != nil {
		t.Fatalf("publish cpu job: %v", err)
	}

	// Give the CPU job time to finish and the GPU job time to wrongly start.
	time.Sleep(1 * time.Second)
	if !saw("cpu") {
		t.Error("CPU job did not run while the GPU queue was paused")
	}
	if saw("gpu") {
		t.Error("GPU job ran while its queue was paused")
	}

	// Game ends: resume the GPU queue; the held job runs.
	lw.Resume(gpuQ)
	waitWG(t, &wg, 10*time.Second, "GPU job to run after resume")
}
