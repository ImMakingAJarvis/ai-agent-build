// Package engine manages the Jarvis engine (llama.cpp) systemd service
// lifecycle — starting and stopping it in response to GPU contention.
package engine

import (
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Manager controls the jarvis-engine systemd service.
type Manager struct {
	serviceName string

	mu          sync.Mutex
	running     bool // whether we believe the service is running
	stopping    bool // whether a stop is in progress
	startCh     chan struct{} // closed when start completes
	stopCh      chan struct{} // closed when stop completes
	stopped     atomic.Bool   // true when engine has been stopped
}

// New creates a Manager for the given systemd service name.
func New(serviceName string) *Manager {
	return &Manager{
		serviceName: serviceName,
		startCh:   make(chan struct{}),
		stopCh:    make(chan struct{}),
	}
}

// IsRunning returns whether the manager believes the engine is running.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// IsStopped returns whether the engine has been stopped (and not restarted).
func (m *Manager) IsStopped() bool {
	return m.stopped.Load()
}

// StartService starts the systemd service and waits for it to become ready
// (health check on port 11434 passes). Returns an error if starting or
// health-checking fails.
func (m *Manager) StartService() error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = true
	m.stopped.Store(false)
	m.mu.Unlock()

	log.Printf("engine: starting %s", m.serviceName)
	if err := m.runCmd("systemctl", "start", m.serviceName); err != nil {
		log.Printf("engine: start %s: %v", m.serviceName, err)
		return err
	}

	// Wait for the engine to become healthy.
	if err := m.waitForHealthy(30 * time.Second); err != nil {
		log.Printf("engine: %s did not become healthy: %v", m.serviceName, err)
		return err
	}

	close(m.startCh)
	log.Printf("engine: %s started and healthy", m.serviceName)
	return nil
}

// StopService stops the systemd service.
func (m *Manager) StopService() error {
	m.mu.Lock()
	if !m.running || m.stopping {
		m.mu.Unlock()
		return nil
	}
	m.stopping = true
	m.mu.Unlock()

	log.Printf("engine: stopping %s", m.serviceName)
	if err := m.runCmd("systemctl", "stop", m.serviceName); err != nil {
		log.Printf("engine: stop %s: %v", m.serviceName, err)
		return err
	}

	m.mu.Lock()
	m.running = false
	m.stopping = false
	m.stopped.Store(true)
	m.mu.Unlock()

	close(m.stopCh)
	log.Printf("engine: %s stopped", m.serviceName)
	return nil
}

// AwaitStarted blocks until the service has started and is healthy, or
// returns an error.
func (m *Manager) AwaitStarted() error {
	select {
	case <-m.startCh:
		return nil
	case <-time.After(60 * time.Second):
		return ErrTimeout
	}
}

// AwaitStopped blocks until the service has stopped, or returns an error.
func (m *Manager) AwaitStopped() error {
	select {
	case <-m.stopCh:
		return nil
	case <-time.After(60 * time.Second):
		return ErrTimeout
	}
}

// HealthCheck probes the engine's root endpoint and returns true if it
// responds with 200.
func HealthCheck(addr string) bool {
	// Reuse the ollama client healthcheck pattern.
	cmd := exec.Command("curl", "--max-time", "3", "-s", "-o", "/dev/null",
		"-w", "%{http_code}", addr)
	out, err := cmd.Output()
	return err == nil && string(out) == "200"
}

// ---------- internal ----------

func (m *Manager) runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}
	log.Printf("engine: %s %s: %s", name, args[0], out)
	return nil
}

func (m *Manager) waitForHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("curl", "--max-time", "2", "-s", "-o", "/dev/null",
			"-w", "%{http_code}", "http://localhost:11434/")
		out, err := cmd.Output()
		if err == nil && string(out) == "200" {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return ErrTimeout
}
