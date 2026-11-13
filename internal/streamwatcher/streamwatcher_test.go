package streamwatcher

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------- Config defaults ----------

func TestConfigApplyDefaults(t *testing.T) {
	var cfg Config
	cfg.applyDefaults()
	if cfg.PollInterval != 2*time.Second {
		t.Errorf("PollInterval = %v; want 2s", cfg.PollInterval)
	}
	if cfg.NvidiaEncoderThreshold != 1 {
		t.Errorf("NvidiaEncoderThreshold = %d; want 1", cfg.NvidiaEncoderThreshold)
	}
	if cfg.SunshineUser != "nathan" {
		t.Errorf("SunshineUser = %q; want %q", cfg.SunshineUser, "nathan")
	}
	if cfg.SunshineStreamPort != 48010 {
		t.Errorf("SunshineStreamPort = %d; want 48010", cfg.SunshineStreamPort)
	}
}

func TestConfigPreservesExplicitValues(t *testing.T) {
	cfg := Config{
		PollInterval:           5 * time.Second,
		NvidiaEncoderThreshold: 10,
		SunshineUser:           "alice",
	}
	cfg.applyDefaults()
	if cfg.PollInterval != 5*time.Second {
		t.Errorf("PollInterval = %v; want 5s", cfg.PollInterval)
	}
	if cfg.NvidiaEncoderThreshold != 10 {
		t.Errorf("NvidiaEncoderThreshold = %d; want 10", cfg.NvidiaEncoderThreshold)
	}
	if cfg.SunshineUser != "alice" {
		t.Errorf("SunshineUser = %q; want %q", cfg.SunshineUser, "alice")
	}
}

// ---------- StreamType ----------

func TestStreamTypeString(t *testing.T) {
	tests := []struct {
		st   StreamType
		want string
	}{
		{NoStream, "none"},
		{SunshineStream, "sunshine"},
		{SteamRemotePlay, "steam-remote-play"},
		{SteamGame, "steam-game"},
		{StreamType(99), "none"},
	}
	for _, tt := range tests {
		got := tt.st.String()
		if got != tt.want {
			t.Errorf("StreamType(%d).String() = %q; want %q", tt.st, got, tt.want)
		}
	}
}

// IsGame splits the kinds: a game is a tenant that wants the 4090's VRAM
// (the brain goes out for it); a Sunshine desktop stream is not (NVENC
// only).
func TestIsGame(t *testing.T) {
	tests := []struct {
		st   StreamType
		want bool
	}{
		{NoStream, false},
		{SunshineStream, false},
		{SteamRemotePlay, true},
		{SteamGame, true},
	}
	for _, tt := range tests {
		if got := tt.st.IsGame(); got != tt.want {
			t.Errorf("%v.IsGame() = %v; want %v", tt.st, got, tt.want)
		}
	}
}

// ---------- procGameRunning (the Steam game detection) ----------

// writeCmdline writes one synthetic /proc entry: <root>/<pid>/cmdline with
// NUL-separated argv (what the kernel keeps).
func writeCmdline(t *testing.T, root, pid, cmdline string) {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The detection reads argv, not the exe link: a native game's own binary
// sits at …/steamapps/common/<Game>/, a Proton launch carries the game
// path among the proton arguments, and every library root (default,
// snap, extra libraries) contains the same "steamapps/common/" marker —
// while the Steam client itself and SteamLinuxRuntime never match.
func TestProcGameRunning(t *testing.T) {
	d := t.TempDir()

	// A snap-install Proton launch: the game path is among proton's argv.
	writeCmdline(t, d, "100",
		"/home/nathan/snap/steam/common/.local/share/Steam/steamapps/common/Proton 9-0/proton\x00"+
			"/home/nathan/snap/steam/common/.local/share/Steam/steamapps/common/Fake Game/fakegame\x00")
	// A native game: its own binary is the install path.
	writeCmdline(t, d, "200",
		"/home/nathan/snap/steam/common/.local/share/Steam/steamapps/common/AnotherGame/another\x00")
	// The Steam client itself: no steamapps/common/ in argv.
	writeCmdline(t, d, "300", "/snap/steam/279/ubuntu12_32/steam\x00--version\x00")
	// SteamLinuxRuntime lives in the same tree but is tooling, not a game.
	writeCmdline(t, d, "400",
		"/home/nathan/snap/steam/common/.local/share/Steam/steamapps/common/SteamLinuxRuntime/runtime\x00")
	// An unrelated process.
	writeCmdline(t, d, "500", "/usr/bin/firefox\x00https://example.com\x00")
	// A non-pid entry (like /proc/self, /proc/net) is skipped, even if it
	// would match — and a pid with no cmdline (kernel thread) is skipped.
	if err := os.MkdirAll(filepath.Join(d, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCmdline(t, d, "600", "")
	if err := os.MkdirAll(filepath.Join(d, "700"), 0o755); err != nil {
		t.Fatal(err)
	}

	if !procGameRunning(d, nil) {
		t.Fatal("procGameRunning = false, want true (a Proton + a native game are running)")
	}
	if procGameRunning(d, []string{"Fake Game", "AnotherGame"}) {
		t.Fatal("procGameRunning = true with both games excluded, want false")
	}
	if !procGameRunning(d, []string{"Fake Game"}) {
		t.Fatal("procGameRunning = false excluding only one game, want true (the other is running)")
	}
	// An exclude that matches nothing changes nothing.
	if !procGameRunning(d, []string{"SomeOtherGame"}) {
		t.Fatal("procGameRunning = false with an irrelevant exclude, want true")
	}
	// An empty root reports no game (the walk fails cleanly).
	if procGameRunning(filepath.Join(d, "does-not-exist"), nil) {
		t.Fatal("procGameRunning = true on a missing root, want false")
	}
}

// ---------- isPID ----------

func TestIsPID(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"1", true},
		{"12345", true},
		{"0", true},
		{"abc", false},
		{"", false},
		{"self", false},
		{"net", false},
	}
	for _, tt := range tests {
		got := isPID(tt.s)
		if got != tt.want {
			t.Errorf("isPID(%q) = %v; want %v", tt.s, got, tt.want)
		}
	}
}

// ---------- isStreamSocket (port-scoped UDP check) ----------

// The namespace-wide /proc/<pid>/net/udp table must not fire on any
// connected UDP socket — only on the Sunshine stream port. Sunshine's
// default stream port is 48010 = 0xBB8A; the DHCP client on this box
// keeps a connected socket on ports 68/67 (0x0044/0x0043) at all times.
func TestIsStreamSocket(t *testing.T) {
	// Connected client socket to 127.0.0.1:48010 (the S3 simulation /
	// a local Moonlight client).
	client := "   1: 0100007F:C000 0100007F:BB8A 01 00000000:00000000 00:00000000 00000000     0        0 67890"
	// Sunshine's own unconnected listener on the stream port.
	listener := "   1: 00000000:BB8A 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 12345"
	// The DHCP client's connected socket (the live false positive).
	dhcp := "   1: 1B02016F:0044 0102016F:0043 01 00000000:00000000 00:00000000 00000000     0        0 11111"
	// A connected socket on an unrelated port.
	other := "   1: 0100007F:C001 0A0000AC:1F90 01 00000000:00000000 00:00000000 00000000     0        0 22222"

	if !isStreamSocket(strings.Fields(client), 48010) {
		t.Error("connected client socket on the stream port must count")
	}
	// Also matched when the stream port is on the local end.
	local := "   1: 0100007F:BB8A 0100007F:C002 01 00000000:00000000 00:00000000 00000000     0        0 33333"
	if !isStreamSocket(strings.Fields(local), 48010) {
		t.Error("connected socket with the stream port on the local end must count")
	}
	if isStreamSocket(strings.Fields(listener), 48010) {
		t.Error("Sunshine's own unconnected listener (st 07) must not count")
	}
	if isStreamSocket(strings.Fields(dhcp), 48010) {
		t.Error("a connected DHCP socket must not look like a game stream")
	}
	if isStreamSocket(strings.Fields(other), 48010) {
		t.Error("a connected socket on another port must not count")
	}
}

// ---------- hasSteamRemotePlayConnections (scanner logic) ----------

func TestSteamRemotePlayPortParsing(t *testing.T) {
	// Synthetic /proc/<pid>/net/tcp with an established connection on 27000.
	// 27000 decimal = 0x6978 hex.
	tcpData :=
		" sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
			"   0: 0100007F:6978 0A0000AC:4B20 01 00000000:00000000 00:00000000 00000000     0        0 11111\n"+
			"   0: 0100007F:1F90 0A0000AC:4B21 01 00000000:00000000 00:00000000 00000000     0        0 22222\n"

	found := false
	scanner := bufio.NewScanner(strings.NewReader(tcpData))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || fields[3] != "01" {
			continue
		}
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
			found = true
		}
	}
	if !found {
		t.Error("expected to find Steam Remote Play port in synthetic TCP data")
	}
}

// ---------- nvidiaEncoderActive returns false when nvidia-smi fails ----------

func TestNvidiaEncoderActiveFailsGracefully(t *testing.T) {
	// With no nvidia-smi on PATH every query fails and the watcher reports
	// no stream (a missing tool must not look like a game).
	old := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	if got := nvidiaEncoderActive(1); got != false {
		t.Errorf("nvidiaEncoderActive = %v, want false with no nvidia-smi", got)
	}
	_ = old
}

// TestEncoderFieldActiveFakeSMI drives encoderFieldActive with a fake
// nvidia-smi that answers per field, covering the driver-595+ names
// (utilization.encoder / encoder.stats.sessionCount) and the legacy name
// (encoder.utilization), including a driver that rejects an unknown field
// (the whole query must fail, not parse garbage).
func TestEncoderFieldActiveFakeSMI(t *testing.T) {
	d := t.TempDir()
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    --query-gpu=utilization.encoder) echo "7"; exit 0;;
    --query-gpu=encoder.utilization) echo 'Field "encoder.utilization" is not a valid field to query.'; exit 0;;
    --query-gpu=encoder.stats.sessionCount) echo "1"; exit 0;;
  esac
done
exit 0
`
	if err := os.WriteFile(filepath.Join(d, "nvidia-smi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", d)

	if !encoderFieldActive("utilization.encoder", 1) {
		t.Error("utilization.encoder=7 should exceed threshold 1")
	}
	if encoderFieldActive("utilization.encoder", 99) {
		t.Error("utilization.encoder=7 must not exceed threshold 99")
	}
	if encoderFieldActive("encoder.utilization", 0) {
		t.Error("a driver-rejected field must read as inactive, not garbage")
	}
	if !encoderFieldActive("encoder.stats.sessionCount", 0) {
		t.Error("one encoder session must count as active")
	}
	if got := nvidiaEncoderActive(1); got != true {
		t.Errorf("nvidiaEncoderActive = %v, want true (utilization 7 > 1)", got)
	}
}

// ---------- procName ----------

func TestProcNameFallsBackWhenGone(t *testing.T) {
	// A dead pid has no comm and no exe: the name must come back empty,
	// not an error path.
	if got := procName("99999999"); got != "" {
		t.Errorf("procName(dead pid) = %q; want %q", got, "")
	}
	// Our own pid must resolve to a non-empty command name.
	if got := procName(strconv.Itoa(os.Getpid())); got == "" {
		t.Error("procName(self) must be non-empty")
	}
}

// ---------- Watcher Start/Stop ----------

func TestWatcherStartStop(t *testing.T) {
	ch := make(chan StateChange, 4)
	cfg := Config{PollInterval: 10 * time.Millisecond}
	w := New(cfg)
	w.Start(ch)

	// Give the loop a moment to run at least one tick.
	time.Sleep(50 * time.Millisecond)

	w.Stop()

	// The channel should eventually close and we should be able to drain it.
	close(ch)
	for range ch {
		// drain
	}
}
