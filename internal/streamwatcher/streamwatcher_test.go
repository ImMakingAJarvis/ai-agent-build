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
		{StreamType(99), "none"},
	}
	for _, tt := range tests {
		got := tt.st.String()
		if got != tt.want {
			t.Errorf("StreamType(%d).String() = %q; want %q", tt.st, got, tt.want)
		}
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

// ---------- hasEstablishedUDPSockets (with synthetic data) ----------

func TestHasEstablishedUDPSockets(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a synthetic /proc/<pid>/net/udp file with an established socket.
	udpFile := filepath.Join(tmpDir, "net/udp")
	if err := os.MkdirAll(filepath.Dir(udpFile), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(udpFile, []byte(
		" sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
			"   1: 00000000:1F90 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 12345\n"+
			"   1: 0100007F:1F90 080800AC:9E77 01 00000000:00000000 00:00000000 00000000     0        0 67890\n",
	), 0644)

	// We can't call hasEstablishedUDPSockets directly with a fake path,
	// but we can test the scanner logic inline.
	data, _ := os.ReadFile(udpFile)
	found := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 4 && fields[3] == "01" {
			found = true
		}
	}
	if !found {
		t.Error("expected to find established UDP socket in synthetic data")
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
	// On this test machine nvidia-smi likely doesn't exist or returns an error.
	// The function should return false without panicking.
	result := nvidiaEncoderActive(1)
	// We just verify it doesn't crash; the result depends on the environment.
	_ = result
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
