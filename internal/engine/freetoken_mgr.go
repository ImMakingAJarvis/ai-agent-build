// Package engine manages the Jarvis engine systemd services lifecycle.
package engine

import (
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// FreeTokenManager controls the jarvis-moe-engine systemd service.
type FreeTokenManager struct {
	serviceName string

	mu      sync.Mutex
	running bool // whether we believe the service is running
	stopCh  chan struct{} // closed when stop completes
	stopped atomic.Bool   // true when engine has been stopped
}

// NewFreeTokenManager creates a manager for the FreeToken systemd service.
func NewFreeTokenManager(serviceName string) *FreeTokenManager {
	return &FreeTokenManager{
		serviceName: serviceName,
		stopCh:      make(chan struct{}),
	}
}

// IsRunning returns whether the manager believes the FreeToken engine is running.
func (m *FreeTokenManager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// IsStopped returns whether the FreeToken engine has been stopped.
func (m *FreeTokenManager) IsStopped() bool {
	return m.stopped.Load()
}

// StartService starts the FreeToken systemd service and waits for it to become ready.
func (m *FreeTokenManager) StartService() error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = true
	m.stopped.Store(false)
	m.mu.Unlock()

	log.Printf("freetoken: starting %s", m.serviceName)
	if err := m.runCmd("systemctl", "start", m.serviceName); err != nil {
		log.Printf("freetoken: start %s: %v", m.serviceName, err)
		return err
	}

	// Wait for the FreeToken engine to become healthy.
	if err := m.waitForHealthy(120 * time.Second); err != nil {
		log.Printf("freetoken: %s did not become healthy: %v", m.serviceName, err)
		return err
	}

	log.Printf("freetoken: %s started and healthy", m.serviceName)
	return nil
}

// StopService stops the FreeToken systemd service.
func (m *FreeTokenManager) StopService() error {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = false
	m.mu.Unlock()

	log.Printf("freetoken: stopping %s", m.serviceName)
	if err := m.runCmd("systemctl", "stop", m.serviceName); err != nil {
		log.Printf("freetoken: stop %s: %v", m.serviceName, err)
		return err
	}

	m.stopped.Store(true)
	close(m.stopCh)
	log.Printf("freetoken: %s stopped", m.serviceName)
	return nil
}

// AwaitStopped blocks until the FreeToken service has stopped.
func (m *FreeTokenManager) AwaitStopped() error {
	select {
	case <-m.stopCh:
		return nil
	case <-time.After(120 * time.Second):
		return fmt.Errorf("timeout waiting for freetoken to stop")
	}
}

// FreeTokenHealthCheck probes the FreeToken server's health endpoint.
func FreeTokenHealthCheck(addr string) bool {
	resp, err := http.Get(addr + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ---------- internal ----------

func (m *FreeTokenManager) runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}
	log.Printf("freetoken: %s %s: %s", name, args[0], out)
	return nil
}

func (m *FreeTokenManager) waitForHealthy(timeout time.Duration) error {
	// Read the MOE_ENGINE_URL from environment or use default.
	addr := "http://localhost:11435"
	if url := m.moeEngineURL(); url != "" {
		addr = url
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(addr + "/health")
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("timeout waiting for freetoken health")
}

func (m *FreeTokenManager) moeEngineURL() string {
	// Check environment for the FreeToken server URL.
	return ""
}
