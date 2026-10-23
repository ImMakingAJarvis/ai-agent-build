// Package streamwatcher detects active remote-desktop and game-streaming
// sessions (Sunshine, Steam Remote Play) by polling process and status
// endpoints, then emits stream_start and stream_end events on a shared
// channel so other subsystems can react.
package streamwatcher

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config holds tuning parameters for the watcher.
type Config struct {
	// PollInterval is how often the watcher checks for active streams.
	// Default: 2 seconds.
	PollInterval time.Duration

	// NvidiaEncoderThreshold is the encoder utilization percentage above
	// which we consider NVENC actively encoding. Default: 1.
	NvidiaEncoderThreshold int

	// SunshineUser is the system user that runs the Sunshine daemon.
	// Default: "nathan".
	SunshineUser string

	// SunshineEndpoint is the HTTP API base URL for Sunshine.
	// e.g. "http://127.0.0.1:8080/api/v2". If empty, the watcher falls
	// back to /proc-based detection.
	SunshineEndpoint string

	// SteamEndpoint is the HTTP API base URL for the Steam in-home streaming
	// status page. If empty, the watcher falls back to /proc-based detection.
	SteamEndpoint string
}

func (c *Config) applyDefaults() {
	if c.PollInterval == 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.NvidiaEncoderThreshold == 0 {
		c.NvidiaEncoderThreshold = 1
	}
	if c.SunshineUser == "" {
		c.SunshineUser = "nathan"
	}
}

// StreamType identifies which service is streaming.
type StreamType int

const (
	NoStream StreamType = iota
	SunshineStream
	SteamRemotePlay
)

func (s StreamType) String() string {
	switch s {
	case SunshineStream:
		return "sunshine"
	case SteamRemotePlay:
		return "steam-remote-play"
	default:
		return "none"
	}
}

// StateChange is emitted whenever the streaming state transitions.
// A non-zero Stream value represents a stream_start event;
// a zero (NoStream) value represents a stream_end event.
type StateChange struct {
	Stream StreamType
	Time   time.Time
}

// Watcher periodically checks whether a stream is active and sends
// state transitions on the provided channel.
type Watcher struct {
	cfg   Config
	steam bool // whether Steam is installed

	mu   sync.Mutex
	prev StreamType

	done chan struct{}
}

// New creates a Watcher with the given config.
func New(cfg Config) *Watcher {
	cfg.applyDefaults()
	return &Watcher{
		cfg:  cfg,
		done: make(chan struct{}),
	}
}

// Start begins polling in the background. It sends a StateChange on ch
// whenever the stream state transitions (including the initial state).
func (w *Watcher) Start(ch chan<- StateChange) {
	// Pre-check whether Steam is installed.
	w.steam = steamInstalled()

	log.Printf("streamwatcher: started (poll=%s, sunshine_user=%s)",
		w.cfg.PollInterval, w.cfg.SunshineUser)
	if w.cfg.SunshineEndpoint != "" {
		log.Printf("streamwatcher: sunshine endpoint=%s", w.cfg.SunshineEndpoint)
	}
	if w.cfg.SteamEndpoint != "" {
		log.Printf("streamwatcher: steam endpoint=%s", w.cfg.SteamEndpoint)
	}

	go w.loop(ch)
}

// Stop signals the watcher to shut down its polling loop.
func (w *Watcher) Stop() {
	close(w.done)
	log.Println("streamwatcher: stopped")
}

func (w *Watcher) loop(ch chan<- StateChange) {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			stream := w.detect()
			w.mu.Lock()
			if stream != w.prev {
				if stream != NoStream {
					log.Printf("streamwatcher: stream_start %s", stream)
				} else {
					log.Printf("streamwatcher: stream_end (was %s)", w.prev)
				}
				ch <- StateChange{Stream: stream, Time: time.Now()}
				w.prev = stream
			}
			w.mu.Unlock()
		}
	}
}

// detect runs all checks and returns the most specific active stream type.
func (w *Watcher) detect() StreamType {
	// Sunshine UDP stream connections are the most reliable indicator.
	if sunshineStreaming(w.cfg.SunshineUser) {
		return SunshineStream
	}
	// Steam remote-play established connections.
	if w.steam && steamRemotePlayActive() {
		return SteamRemotePlay
	}
	// Fallback: NVIDIA encoder utilization > 0 means *something* is encoding.
	if nvidiaEncoderActive(w.cfg.NvidiaEncoderThreshold) {
		// Can't tell which service; report sunshine as the likely culprit
		// since it's the primary streaming service on this machine.
		return SunshineStream
	}
	return NoStream
}

// ---------- Sunshine detection ----------

// sunshineStreaming checks whether the Sunshine process has established
// UDP connections (st 01 in /proc/<pid>/net/udp), which indicate an
// active remote-desktop stream.
func sunshineStreaming(user string) bool {
	pids := getPIDsByUser(user, "sunshine")
	if len(pids) == 0 {
		return false
	}
	for _, pid := range pids {
		if hasEstablishedUDPSockets(pid) {
			return true
		}
	}
	return false
}

// ---------- Steam detection ----------

func steamInstalled() bool {
	_, err := exec.LookPath("steam")
	return err == nil
}

// steamRemotePlayActive checks whether the steam process has established
// TCP connections on the Steam Remote Play port range (27000-27009).
func steamRemotePlayActive() bool {
	pids := getPIDs("steam")
	if len(pids) == 0 {
		return false
	}
	for _, pid := range pids {
		if hasSteamRemotePlayConnections(pid) {
			return true
		}
	}
	return false
}

// ---------- NVIDIA encoder detection ----------

// nvidiaEncoderActive queries nvidia-smi for NVENC utilization and
// returns true when it exceeds the configured threshold.
func nvidiaEncoderActive(threshold int) bool {
	out, err := exec.Command(
		"nvidia-smi",
		"--query-gpu=encoder.utilization",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), ",") {
		util, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			continue
		}
		if util > threshold {
			return true
		}
	}
	return false
}

// ---------- /proc helpers ----------

// getPIDsByUser returns PIDs of processes named name owned by user.
func getPIDsByUser(user, name string) []int {
	return filterPIDs(user, func(exe string) bool {
		return strings.Contains(exe, name)
	})
}

// getPIDs returns PIDs of processes with the given executable name.
func getPIDs(name string) []int {
	return filterPIDs("", func(exe string) bool {
		return strings.Contains(exe, name)
	})
}

// filterPIDs walks /proc and returns PIDs matching the predicate.
// If user is non-empty it additionally filters by owner.
func filterPIDs(user string, matchFn func(string) bool) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		if !isPID(e.Name()) {
			continue
		}
		// Check ownership if user filter is set.
		if user != "" {
			uid := getProcUID(e.Name())
			if uid != user {
				continue
			}
		}
		exe := getProcExe("/proc/" + e.Name() + "/exe")
		if matchFn(exe) {
			pid, _ := strconv.Atoi(e.Name())
			pids = append(pids, pid)
		}
	}
	return pids
}

func isPID(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// getProcUID returns the username of the process owner.
func getProcUID(pidStr string) string {
	status, err := os.ReadFile("/proc/" + pidStr + "/status")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			uid, _ := strconv.Atoi(strings.Fields(line)[1])
			// Look up username from /etc/passwd.
			return lookupUser(uid)
		}
	}
	return ""
}

func lookupUser(uid int) string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 7)
		if len(parts) < 3 {
			continue
		}
		lineUID, _ := strconv.Atoi(parts[2])
		if lineUID == uid {
			return parts[0]
		}
	}
	return ""
}

func getProcExe(linkPath string) string {
	target, err := os.Readlink(linkPath)
	if err != nil {
		return ""
	}
	return target
}

// hasEstablishedUDPSockets returns true if the process has any UDP
// sockets in state 01 (ESTABLISHED).
func hasEstablishedUDPSockets(pid int) bool {
	path := fmt.Sprintf("/proc/%d/net/udp", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 4 && fields[3] == "01" {
			return true
		}
	}
	return false
}

// hasSteamRemotePlayConnections returns true if the process has
// established TCP connections on port 27000 (0x6978) or nearby
// Steam Remote Play ports (27000-27009 = 0x6978-0x6981).
func hasSteamRemotePlayConnections(pid int) bool {
	path := fmt.Sprintf("/proc/%d/net/tcp", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		if fields[3] != "01" {
			continue // not ESTABLISHED
		}
		// local_address is hex IP:PORT
		local := fields[1]
		colon := strings.LastIndex(local, ":")
		if colon == -1 {
			continue
		}
		portHex := local[colon+1:]
		port, err := strconv.ParseInt(portHex, 16, 16)
		if err != nil {
			continue
		}
		if port >= 27000 && port <= 27009 {
			return true
		}
	}
	return false
}
