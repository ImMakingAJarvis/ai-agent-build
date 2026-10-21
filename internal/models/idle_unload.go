// Package models provides an idle-timeout unloader for the pinned local model.
package models

import (
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// IdleUnload watches the local model's last-used timestamp and unloads it
// after a configurable period of inactivity.
type IdleUnload struct {
	client       *Client
	model        string
	idleTimeout  time.Duration
	lastUsed     atomic.Int64 // unix nano
	mu           sync.Mutex
	ticker       *time.Ticker
	stopCh       chan struct{}
	checkRunning bool
}

// NewIdleUnload creates an idle unloader for the given model.
// Reads IDLE_TIMEOUT_SECONDS from environment (default 1800 = 30 min).
func NewIdleUnload(client *Client, model string) *IdleUnload {
	seconds := 1800
	if v := os.Getenv("IDLE_TIMEOUT_SECONDS"); v != "" {
		fmt.Sscanf(v, "%d", &seconds)
	}
	return &IdleUnload{
		client:      client,
		model:       model,
		idleTimeout: time.Duration(seconds) * time.Second,
	}
}

// MarkUsed records the current time as the last activity.
// Call this after every successful local-model request.
func (iu *IdleUnload) MarkUsed() {
	iu.lastUsed.Store(time.Now().UnixNano())
}

// Start begins the idle-check loop. Call MarkUsed to keep the model loaded.
func (iu *IdleUnload) Start() {
	iu.mu.Lock()
	defer iu.mu.Unlock()
	if iu.checkRunning {
		return
	}
	iu.checkRunning = true
	iu.stopCh = make(chan struct{})
	iu.ticker = time.NewTicker(30 * time.Second)
	iu.MarkUsed() // start as "now used"

	go func() {
		for {
			select {
			case <-iu.stopCh:
				iu.ticker.Stop()
				log.Printf("idle-unload: stopped")
				return
			case <-iu.ticker.C:
				iu.check()
			}
		}
	}()
	log.Printf("idle-unload: monitoring %s (timeout=%v)", iu.model, iu.idleTimeout)
}

// Stop shuts down the idle-check loop.
func (iu *IdleUnload) Stop() {
	iu.mu.Lock()
	defer iu.mu.Unlock()
	if iu.checkRunning {
		close(iu.stopCh)
		iu.checkRunning = false
	}
}

func (iu *IdleUnload) check() {
	elapsed := time.Since(time.Unix(0, iu.lastUsed.Load()))
	if elapsed > iu.idleTimeout {
		log.Printf("idle-unload: %s idle for %v, unloading", iu.model, elapsed.Round(time.Second))
		if err := iu.client.UnloadModel(iu.model); err != nil {
			log.Printf("idle-unload: unload %s: %v", iu.model, err)
		}
	}
}
