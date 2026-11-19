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
	"path/filepath"
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

	// SunshineStreamPort is the UDP port Sunshine listens on for its
	// video stream. A connected (st 01) UDP socket touching this port —
	// either end — is treated as an active stream client.
	// Default: 48010 (Sunshine's default).
	SunshineStreamPort int

	// SunshineEndpoint is the HTTP API base URL for Sunshine.
	// e.g. "http://127.0.0.1:8080/api/v2". If empty, the watcher falls
	// back to /proc-based detection.
	SunshineEndpoint string

	// SteamEndpoint is the HTTP API base URL for the Steam in-home streaming
	// status page. If empty, the watcher falls back to /proc-based detection.
	SteamEndpoint string

	// SteamGameExcludes is a list of command-line substrings that opt a
	// process out of the game detection (STEAM_GAME_EXCLUDES,
	// comma-separated): a light game the owner does not want to evict the
	// brain for. Empty (default) = every Steam game counts.
	SteamGameExcludes []string
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
	if c.SunshineStreamPort == 0 {
		c.SunshineStreamPort = 48010
	}
}

// StreamType identifies which service is streaming.
type StreamType int

const (
	NoStream StreamType = iota
	SunshineStream
	SteamRemotePlay
	SteamGame
)

func (s StreamType) String() string {
	switch s {
	case SunshineStream:
		return "sunshine"
	case SteamRemotePlay:
		return "steam-remote-play"
	case SteamGame:
		return "steam-game"
	default:
		return "none"
	}
}

// IsGame reports whether the stream is one a game tenant is behind: a
// local Steam game, or Steam Remote Play (the game runs on this host,
// the other side just watches). A Sunshine stream is NOT a game: on
// this box Sunshine is used for plain desktop streaming, which needs
// only NVENC (a few hundred MB of VRAM) and coexists with the brain
// and the capability workers — so the GPU actions (lease revocation,
// the GPU hold, the brain stop) fire only for the game kinds.
func (s StreamType) IsGame() bool {
	return s == SteamGame || s == SteamRemotePlay
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

	// lastGamePID is the pid of the last logged game match, so a game
	// is logged once when it is first seen, not on every poll.
	lastGamePID string

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
	// A running Steam game is the strongest signal — its process tree
	// carries the install path in argv — and it is the state that stops
	// the brain, so it is checked first (a game streamed to a Moonlight
	// client would otherwise read as a Sunshine stream).
	if w.steam {
		if m, ok := steamGameRunning(w.cfg.SteamGameExcludes); ok {
			if m.PID != w.lastGamePID {
				log.Printf("streamwatcher: steam game detected: pid=%s argv0=%q reason=%s",
					m.PID, m.Argv0, m.Reason)
				w.lastGamePID = m.PID
			}
			return SteamGame
		}
		w.lastGamePID = ""
	}
	// Sunshine UDP stream connections are the most reliable indicator.
	if sunshineStreaming(w.cfg.SunshineUser, w.cfg.SunshineStreamPort) {
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

// sunshineStreaming reports whether a Sunshine process owned by user has
// a connected (st 01) UDP socket touching the stream port.
//
// /proc/<pid>/net/udp is the whole network namespace, not the process's
// own sockets, so the state-01 check must be scoped to Sunshine's stream
// port: any other connected UDP socket on the box (DHCP, mDNS, an
// unrelated client) is not a game and would otherwise hold the GPU
// forever. Sunshine keeps its own socket unconnected while it waits for
// clients, so the connected socket we see is the client side of a stream
// — Moonlight connects to the video port the moment a session starts,
// which is what a local (or relaying) client does on this host.
func sunshineStreaming(user string, streamPort int) bool {
	pids := getPIDsByUser(user, "sunshine")
	if len(pids) == 0 {
		return false
	}
	for _, pid := range pids {
		if hasEstablishedUDPOnPort(pid, streamPort) {
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

// gameMatch says which process made procGameRunning report a game and why.
type gameMatch struct {
	PID    string
	Argv0  string
	Reason string
}

// steamGameRunning reports whether a Steam game is running on the host,
// and which process matched.
//
// It reads /proc/<pid>/cmdline (world-readable, so it works from
// resourced as the agent user) and, where readable, the /proc/<pid>/exe
// link. A process counts as a game in exactly two shapes:
//
//   - its own binary is a game: argv[0] or the resolved exe lies inside
//     …/steamapps/common/<Game>/ and <Game> is not a Steam tool dir
//     (Proton*, SteamLinuxRuntime*, Steamworks Shared, …). This is a
//     native game, or a Wine-run game whose argv[0] is its Windows path.
//   - it is a Proton/Wine/Steam launch wrapper (proton, wine*, reaper,
//     pressure-vessel, steam-launch-wrapper, anything run from a tool
//     dir) and a LATER argument is an executable path inside a game dir.
//
// Only whole argv elements count, and only ones that look like a path:
// absolute (or a Windows drive path), no newline, at most PATH_MAX long.
// A process that merely mentions a game path — a prompt passed with
// `claude -p`, an editor, a grep — is neither shape and never matches,
// which is what the old substring scan over the joined cmdline got
// wrong. Every library root carries the same "steamapps/common/" marker
// (~/.local/share/Steam, the snap install, extra libraries such as
// /mnt/ssd1t/SteamLibrary), so the check does not depend on where Steam
// lives. The Steam client and steamwebhelper run from …/ubuntu12_32/
// and …/steamrt64/ and never match.
//
// excludes are substring opt-outs over the whole cmdline
// (Config.SteamGameExcludes) for games the owner does not want to evict
// the brain for.
func steamGameRunning(excludes []string) (gameMatch, bool) {
	return findGame("/proc", excludes)
}

// procGameRunning is findGame without the match details.
func procGameRunning(root string, excludes []string) bool {
	_, ok := findGame(root, excludes)
	return ok
}

// findGame is the /proc walk behind steamGameRunning, split out so the
// tests can point it at a fixture directory.
func findGame(root string, excludes []string) (gameMatch, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return gameMatch{}, false
	}
	for _, e := range entries {
		if !isPID(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "cmdline"))
		if err != nil || len(b) == 0 {
			continue
		}
		if len(b) > 65536 {
			b = b[:65536] // a pathologically long argument list is not a game
		}
		argv := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		exe, _ := os.Readlink(filepath.Join(root, e.Name(), "exe"))
		reason, ok := classifyGame(argv, exe)
		if !ok || excluded(string(b), excludes) {
			continue
		}
		return gameMatch{PID: e.Name(), Argv0: argv[0], Reason: reason}, true
	}
	return gameMatch{}, false
}

// classifyGame reports whether one process (its argv and resolved exe,
// "" when unreadable) is a running Steam game, with the match reason.
func classifyGame(argv []string, exe string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	if game, ok := gameDirOf(argv[0]); ok {
		return "argv0 in game dir " + game, true
	}
	if game, ok := gameDirOf(exe); ok {
		return "exe in game dir " + game, true
	}
	kind := launcherKind(argv, exe)
	if kind == "" {
		return "", false
	}
	for _, arg := range argv[1:] {
		if game, ok := gameDirOf(arg); ok && isExecutablePath(arg) {
			return kind + " launch of " + game + " (" + arg + ")", true
		}
	}
	return "", false
}

// maxPathLen is PATH_MAX: an argument longer than that is not a path.
const maxPathLen = 4096

// gameDirOf reports the <Game> of a path inside …/steamapps/common/<Game>/
// when <Game> is a game, not a Steam tool dir. The path must be a single
// path-shaped argument and must lie INSIDE the game dir (the dir itself
// is not a running game). Windows paths (Wine argv) are normalised.
func gameDirOf(p string) (string, bool) {
	if p == "" || len(p) > maxPathLen || strings.ContainsAny(p, "\n\r") {
		return "", false
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if !strings.HasPrefix(p, "/") && !isDrivePath(p) {
		return "", false
	}
	const marker = "steamapps/common/"
	i := strings.Index(p, marker)
	if i < 0 {
		return "", false
	}
	rest := p[i+len(marker):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 || slash == len(rest)-1 {
		return "", false
	}
	game := rest[:slash]
	if isSteamToolDir(game) {
		return "", false
	}
	return game, true
}

// isDrivePath reports a Windows drive path such as "S:/steamapps/…".
func isDrivePath(p string) bool {
	return len(p) >= 3 && p[1] == ':' && p[2] == '/' &&
		(p[0] >= 'A' && p[0] <= 'Z' || p[0] >= 'a' && p[0] <= 'z')
}

// steamToolDirs are steamapps/common/ entries that ship Steam tooling,
// not games (matched exactly; Proton* and SteamLinuxRuntime* by prefix).
var steamToolDirs = map[string]bool{
	"Steamworks Shared":        true,
	"Steam Controller Configs": true,
	"Steam.dll":                true,
}

func isSteamToolDir(game string) bool {
	return strings.HasPrefix(game, "Proton") ||
		strings.HasPrefix(game, "SteamLinuxRuntime") ||
		steamToolDirs[game]
}

// launcherKind names the Proton/Wine/Steam launch wrapper a process is,
// or "" when it is none of them.
func launcherKind(argv []string, exe string) string {
	for _, p := range []string{argv[0], exe} {
		if inSteamToolDir(p) {
			return "steam-tool"
		}
	}
	base := filepath.Base(strings.ReplaceAll(argv[0], `\`, "/"))
	switch {
	case base == "proton":
		return "proton"
	case strings.HasPrefix(base, "wine"):
		return "wine"
	case strings.HasPrefix(base, "python") && len(argv) > 1 && filepath.Base(argv[1]) == "proton":
		return "proton"
	case base == "reaper", base == "steam-launch-wrapper",
		base == "pv-adverb", base == "pv-bwrap", base == "srt-bwrap",
		base == "pressure-vessel-wrap", base == "_v2-entry-point":
		return "steam-launch"
	}
	return ""
}

// inSteamToolDir reports a path inside …/steamapps/common/<tool>/.
func inSteamToolDir(p string) bool {
	if p == "" || len(p) > maxPathLen || strings.ContainsAny(p, "\n\r") || !strings.HasPrefix(p, "/") {
		return false
	}
	const marker = "steamapps/common/"
	i := strings.Index(p, marker)
	if i < 0 {
		return false
	}
	rest := p[i+len(marker):]
	if slash := strings.IndexByte(rest, '/'); slash > 0 {
		rest = rest[:slash]
	}
	return isSteamToolDir(rest)
}

// isExecutablePath reports whether a game-dir argument names something
// a launcher runs: a Windows executable, or a file with an exec bit.
func isExecutablePath(p string) bool {
	if strings.HasSuffix(strings.ToLower(p), ".exe") {
		return true
	}
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

// excluded reports whether the raw cmdline matches a STEAM_GAME_EXCLUDES
// substring (NULs read as spaces, as an owner would type the command).
func excluded(cmdline string, excludes []string) bool {
	cl := strings.ReplaceAll(cmdline, "\x00", " ")
	for _, x := range excludes {
		if x != "" && strings.Contains(cl, x) {
			return true
		}
	}
	return false
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

// nvidiaEncoderActive queries nvidia-smi for NVENC activity and returns
// true when the encoder engine utilization exceeds the configured
// threshold or an encoder session exists.
//
// The field names moved between driver generations: older drivers expose
// "encoder.utilization"; driver 595+ exposes "utilization.encoder" (and
// "encoder.stats.sessionCount" for active sessions). A query naming a
// field the driver does not know fails as a whole, so each field gets its
// own query and the results are unioned — the query the driver understands
// wins, the others are skipped.
func nvidiaEncoderActive(threshold int) bool {
	for _, field := range []string{"utilization.encoder", "encoder.utilization"} {
		if encoderFieldActive(field, threshold) {
			return true
		}
	}
	return encoderFieldActive("encoder.stats.sessionCount", 0) // any session
}

// encoderFieldActive queries one gpu-scope field and reports whether any
// GPU's value is above min. Unparseable lines (the driver rejected the
// field) are skipped.
func encoderFieldActive(field string, min int) bool {
	out, err := exec.Command(
		"nvidia-smi",
		"--query-gpu="+field,
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		v, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			continue
		}
		if v > min {
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
//
// Matching uses the process name (see procName), not the exe path:
// reading /proc/<pid>/exe is ptrace-gated, so an unprivileged daemon
// (resourced runs as agent) cannot see processes owned by other users
// via exe and the check would silently match nothing.
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
		if matchFn(procName(e.Name())) {
			pid, _ := strconv.Atoi(e.Name())
			pids = append(pids, pid)
		}
	}
	return pids
}

// procName returns the process command name from /proc/<pid>/comm
// (world-readable, truncated to 15 characters — plenty for "sunshine"
// and "steam"), falling back to the exe link target if comm is gone.
func procName(pidStr string) string {
	if data, err := os.ReadFile("/proc/" + pidStr + "/comm"); err == nil {
		if name := strings.TrimSpace(string(data)); name != "" {
			return name
		}
	}
	return getProcExe("/proc/" + pidStr + "/exe")
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

// hasEstablishedUDPOnPort reports whether the UDP table seen from pid
// contains a connected (st 01) socket touching port on either end.
func hasEstablishedUDPOnPort(pid, port int) bool {
	path := fmt.Sprintf("/proc/%d/net/udp", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		if isStreamSocket(strings.Fields(scanner.Text()), port) {
			return true
		}
	}
	return false
}

// isStreamSocket reports whether one /proc/net/udp line is a connected
// (st 01) UDP socket touching port. Line fields: sl local_address
// rem_address st ...; addresses are hex IP:PORT.
func isStreamSocket(fields []string, port int) bool {
	if len(fields) < 4 || fields[3] != "01" {
		return false
	}
	for _, addr := range []string{fields[1], fields[2]} {
		colon := strings.LastIndex(addr, ":")
		if colon == -1 {
			continue
		}
		p, err := strconv.ParseInt(addr[colon+1:], 16, 32)
		if err == nil && int(p) == port {
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
