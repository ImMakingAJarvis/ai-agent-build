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

const (
	steamHome  = "/home/nathan/.local/share/Steam/steamapps/common/"
	ssdLibrary = "/mnt/ssd1t/SteamLibrary/steamapps/common/"
	protonDir  = steamHome + "Proton - Experimental/"
	riftApart  = steamHome + "Ratchet & Clank - Rift Apart/RiftApart.exe"
)

// writeProc writes one synthetic /proc entry: argv as a NUL-separated
// cmdline and, when exe is set, the exe link (dangling is fine — only
// the link target is read).
func writeProc(t *testing.T, root, pid string, argv []string, exe string) {
	t.Helper()
	writeCmdline(t, root, pid, strings.Join(argv, "\x00")+"\x00")
	if exe != "" {
		if err := os.Symlink(exe, filepath.Join(root, pid, "exe")); err != nil {
			t.Fatal(err)
		}
	}
}

// A process is a game only when its own binary sits in a game dir, or
// when a Proton/Wine/Steam launch wrapper is handed a game executable as
// a whole argument. Mentioning a game path (a prompt, a grep) is not a
// game, and neither is Steam's own tooling.
func TestClassifyGame(t *testing.T) {
	cases := []struct {
		name   string
		argv   []string
		exe    string
		want   bool
		reason string
	}{
		{
			name:   "native game",
			argv:   []string{steamHome + "Slay the Spire 2/SlayTheSpire2", "--fullscreen"},
			want:   true,
			reason: "argv0 in game dir Slay the Spire 2",
		},
		{
			name:   "native game by exe link",
			argv:   []string{"./SlayTheSpire2"},
			exe:    steamHome + "Slay the Spire 2/SlayTheSpire2",
			want:   true,
			reason: "exe in game dir Slay the Spire 2",
		},
		{
			name:   "game in the extra SSD library",
			argv:   []string{ssdLibrary + "Baldurs Gate 3/bin/bg3"},
			want:   true,
			reason: "argv0 in game dir Baldurs Gate 3",
		},
		{
			name:   "proton script launching RiftApart.exe",
			argv:   []string{"python3", protonDir + "proton", "waitforexitandrun", riftApart},
			exe:    "/usr/bin/python3.12",
			want:   true,
			reason: "proton launch of Ratchet & Clank - Rift Apart",
		},
		{
			name: "reaper wrapping the proton launch",
			argv: []string{"/home/nathan/.local/share/Steam/steamrt64/reaper", "SteamLaunch", "AppId=1895880", "--",
				steamHome + "SteamLinuxRuntime_4/_v2-entry-point", "--verb=waitforexitandrun", "--",
				protonDir + "proton", "waitforexitandrun", riftApart},
			want:   true,
			reason: "steam-launch launch of Ratchet & Clank - Rift Apart",
		},
		{
			name: "pressure-vessel adverb wrapping the proton launch",
			argv: []string{"/usr/lib/pressure-vessel/from-host/libexec/steam-runtime-tools-0/pv-adverb",
				"--generate-locales", "--", protonDir + "proton", "waitforexitandrun", riftApart},
			want:   true,
			reason: "steam-launch launch of Ratchet & Clank - Rift Apart",
		},
		{
			name:   "the game under wine (windows argv0)",
			argv:   []string{`S:\steamapps\common\Ratchet & Clank - Rift Apart\RiftApart.exe`},
			exe:    protonDir + "files/bin/wine64-preloader",
			want:   true,
			reason: "argv0 in game dir Ratchet & Clank - Rift Apart",
		},
		{
			name:   "wine handed a game exe",
			argv:   []string{"wine64", ssdLibrary + "BioShock Infinite/Binaries/Win32/BioShockInfinite.exe"},
			want:   true,
			reason: "wine launch of BioShock Infinite",
		},
		{
			name: "claude -p prompt that mentions steamapps/common",
			argv: []string{"claude", "--model", "jarvis-brain", "-p",
				"=== YOUR JOB THIS ROUND: Fix steam's download issue ===\n1. Exit Steam completely.\n" +
					"2. rm -rf ~/.local/share/Steam/steamapps/common/Proton\\ Experimental\n" +
					"3. check " + riftApart + "\n"},
			want: false,
		},
		{
			name: "a non-launcher handed a game path as its own argument",
			argv: []string{"grep", "-r", "crash", riftApart},
			want: false,
		},
		{
			name: "steamwebhelper",
			argv: []string{"/home/nathan/.local/share/Steam/ubuntu12_64/steamwebhelper", "--type=zygote",
				"--user-data-dir=/home/nathan/.local/share/Steam/config/htmlcache"},
			want: false,
		},
		{
			name: "the steam client",
			argv: []string{"/home/nathan/.local/share/Steam/steamrt64/steam", "-srt-logger-opened"},
			want: false,
		},
		{
			name: "proton install dir on its own: wineserver",
			argv: []string{protonDir + "files/lib/wine/../../bin/wineserver"},
			want: false,
		},
		{
			name: "proton with no game argument",
			argv: []string{protonDir + "proton", "runinprefix", "winecfg"},
			want: false,
		},
		{
			name: "steam linux runtime",
			argv: []string{steamHome + "SteamLinuxRuntime_sniper/pressure-vessel/bin/pv-bwrap", "--args", "26"},
			want: false,
		},
		{
			name: "a launcher pointing at a game dir, not an executable in it",
			argv: []string{"wine64", ssdLibrary + "BioShock Infinite/"},
			want: false,
		},
		{
			name: "a launcher argument too long to be a path",
			argv: []string{"wine64", ssdLibrary + "Game/" + strings.Repeat("a", maxPathLen) + ".exe"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, got := classifyGame(tc.argv, tc.exe)
			if got != tc.want {
				t.Fatalf("classifyGame = %v (%q), want %v", got, reason, tc.want)
			}
			if tc.want && !strings.HasPrefix(reason, tc.reason) {
				t.Fatalf("reason = %q, want prefix %q", reason, tc.reason)
			}
		})
	}
}

// The /proc walk: the claude round and Steam's helpers alone are no game;
// a real game is found with its pid, argv0 and reason; STEAM_GAME_EXCLUDES
// still opts games out.
func TestProcGameRunning(t *testing.T) {
	d := t.TempDir()
	writeProc(t, d, "100", []string{"claude", "-p", "rm -rf " + protonDir + "\nthen restart steam"}, "")
	writeProc(t, d, "200", []string{"/home/nathan/.local/share/Steam/ubuntu12_64/steamwebhelper", "--type=zygote"}, "")
	writeProc(t, d, "300", []string{protonDir + "files/bin/wineserver"}, "")
	// A non-pid entry and pids without a cmdline are skipped.
	if err := os.MkdirAll(filepath.Join(d, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCmdline(t, d, "600", "")
	if err := os.MkdirAll(filepath.Join(d, "700"), 0o755); err != nil {
		t.Fatal(err)
	}

	if m, ok := findGame(d, nil); ok {
		t.Fatalf("findGame = %+v with no game running, want none", m)
	}

	writeProc(t, d, "400", []string{ssdLibrary + "Baldurs Gate 3/bin/bg3"}, "")
	m, ok := findGame(d, nil)
	if !ok {
		t.Fatal("findGame = none, want the BG3 process")
	}
	if m.PID != "400" || m.Argv0 != ssdLibrary+"Baldurs Gate 3/bin/bg3" || !strings.Contains(m.Reason, "Baldurs Gate 3") {
		t.Fatalf("findGame = %+v, want pid 400, the bg3 argv0 and a BG3 reason", m)
	}

	writeProc(t, d, "500", []string{"python3", protonDir + "proton", "waitforexitandrun", riftApart}, "")
	if !procGameRunning(d, nil) {
		t.Fatal("procGameRunning = false, want true")
	}
	if procGameRunning(d, []string{"Baldurs Gate 3", "Rift Apart"}) {
		t.Fatal("procGameRunning = true with both games excluded, want false")
	}
	if !procGameRunning(d, []string{"Baldurs Gate 3"}) {
		t.Fatal("procGameRunning = false excluding only BG3, want true (Rift Apart is running)")
	}
	if !procGameRunning(d, []string{"SomeOtherGame"}) {
		t.Fatal("procGameRunning = false with an irrelevant exclude, want true")
	}
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
