package power

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeGPU writes a shell script that answers nvidia-smi's power query
// with the given watts.
func fakeGPU(t *testing.T, watts string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-gpu")
	script := "#!/bin/sh\nprintf '%s\\n' " + `"` + watts + `"` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JARVIS_POWER_GPU_CMD", path)
	return path
}

func TestPowerDraw(t *testing.T) {
	fakeGPU(t, "364.53")
	w, err := PowerDraw()
	if err != nil {
		t.Fatal(err)
	}
	if w != 364.53 {
		t.Fatalf("power = %v, want 364.53", w)
	}
}

func TestPowerDrawMultiGPU(t *testing.T) {
	fakeGPU(t, "100.0, 50.0")
	w, err := PowerDraw()
	if err != nil {
		t.Fatal(err)
	}
	if w != 100.0 {
		t.Fatalf("power = %v, want 100 (first GPU)", w)
	}
}

func TestPowerDrawMissingCmd(t *testing.T) {
	t.Setenv("JARVIS_POWER_GPU_CMD", filepath.Join(t.TempDir(), "nope"))
	if _, err := PowerDraw(); err == nil {
		t.Fatal("want error for missing command")
	}
}

func TestRateAndSystemWatts(t *testing.T) {
	t.Setenv("JARVIS_POWER_RATE", "0.15")
	if got := Rate(); got != 0.15 {
		t.Fatalf("rate = %v, want 0.15", got)
	}
	t.Setenv("JARVIS_POWER_RATE", "garbage")
	if got := Rate(); got != 0 {
		t.Fatalf("rate = %v, want 0 for garbage", got)
	}
	t.Setenv("JARVIS_POWER_SYSTEM_W", "60")
	if got := SystemWatts(); got != 60 {
		t.Fatalf("system watts = %v, want 60", got)
	}
	t.Setenv("JARVIS_POWER_SAMPLE_SECS", "0.05")
	if got := Interval(); got != 50*time.Millisecond {
		t.Fatalf("interval = %v, want 50ms", got)
	}
}

func TestKwhAndCost(t *testing.T) {
	if got := Kwh(3600); got != 3.6 {
		t.Fatalf("kwh = %v, want 3.6", got)
	}
	// 3600 Wh at $0.25/kWh = $0.90
	if got := Cost(3600, 0.25); got != 0.9 {
		t.Fatalf("cost = %v, want 0.9", got)
	}
	if got := Cost(3600, 0); got != 0 {
		t.Fatalf("cost at rate 0 = %v, want 0", got)
	}
}

func TestSamplerAccumulatesAndPersists(t *testing.T) {
	fakeGPU(t, "1000") // 1 kW constant
	dir := t.TempDir()
	s := NewSampler(dir)
	s.interval = 20 * time.Millisecond
	s.Start()
	time.Sleep(200 * time.Millisecond)
	wh := s.Stop()

	// ~1 kW for ~0.2 s -> ~0.056 Wh. Allow slack for timing and the
	// first-tick boundary.
	if wh < 0.02 || wh > 0.15 {
		t.Fatalf("wh = %v, want ~0.056", wh)
	}
	if s.Stop() != wh {
		t.Fatal("second Stop must return the same total")
	}
	persisted, err := LoadPersisted(dir)
	if err != nil {
		t.Fatalf("persisted file missing: %v", err)
	}
	if persisted != wh {
		t.Fatalf("persisted %v != returned %v", persisted, wh)
	}
}

func TestSamplerAccumulatesAcrossRestarts(t *testing.T) {
	fakeGPU(t, "1000")
	dir := t.TempDir()
	s1 := NewSampler(dir)
	s1.interval = 20 * time.Millisecond
	s1.Start()
	time.Sleep(100 * time.Millisecond)
	wh1 := s1.Stop()

	// A new sampler in the same dir continues from the persisted total.
	s2 := NewSampler(dir)
	s2.interval = 20 * time.Millisecond
	s2.Start()
	time.Sleep(100 * time.Millisecond)
	wh2 := s2.Stop()
	if wh2 <= wh1 {
		t.Fatalf("restart total %v did not grow past %v", wh2, wh1)
	}
}

func TestSamplerAddsSystemWatts(t *testing.T) {
	fakeGPU(t, "0") // GPU draws nothing; only the flat overhead counts
	t.Setenv("JARVIS_POWER_SYSTEM_W", "50")
	dir := t.TempDir()
	s := NewSampler(dir)
	s.interval = 20 * time.Millisecond
	s.Start()
	time.Sleep(200 * time.Millisecond)
	wh := s.Stop()
	if wh < 0.001 || wh > 0.01 { // 50 W * ~0.2 s = 0.0028 Wh, with slack
		t.Fatalf("wh = %v, want ~0.0028 (50 W for ~0.2 s)", wh)
	}
}

func TestSamplerSurvivesFailedReads(t *testing.T) {
	// Command that fails on the first call, succeeds afterwards.
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	path := filepath.Join(dir, "fake-gpu")
	script := `#!/bin/sh
if [ -f "` + state + `" ]; then
  printf '400.0\n'
else
  touch "` + state + `"
  exit 1
fi
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JARVIS_POWER_GPU_CMD", path)
	s := NewSampler(dir)
	s.interval = 20 * time.Millisecond
	s.Start()
	time.Sleep(150 * time.Millisecond)
	wh := s.Stop()
	if wh <= 0 {
		t.Fatal("want energy despite the first failed read")
	}
}

func TestLoadPersistedMissing(t *testing.T) {
	if _, err := LoadPersisted(t.TempDir()); err == nil {
		t.Fatal("want error for missing power.json")
	}
}
