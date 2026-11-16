package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/fit"
)

func TestBuildSelfReport(t *testing.T) {
	rep := buildSelfReport("node2", fit.Readings{
		FreeVRAMGB: 6, FreeRAMGB: 12, TotalVRAMGB: 24, TotalRAMGB: 32,
	})
	if rep.Device != "remote:node2" {
		t.Errorf("device = %q", rep.Device)
	}
	if rep.VRAMTotalMB != 24*1024 || rep.RAMTotalMB != 32*1024 || rep.RAMFreeMB != 12*1024 {
		t.Errorf("report = %+v (GiB→MB)", rep)
	}
}

func TestPostSelfReport(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/device-report" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := postSelfReport(context.Background(), srv.URL, selfReport{
		Device: "remote:node2", VRAMTotalMB: 24576, RAMTotalMB: 32768, RAMFreeMB: 12288,
	})
	if err != nil {
		t.Fatalf("postSelfReport: %v", err)
	}
	if got["device"] != "remote:node2" || got["vram_total_mb"] != float64(24576) ||
		got["ram_total_mb"] != float64(32768) || got["ram_free_mb"] != float64(12288) {
		t.Errorf("payload = %v", got)
	}
}

func TestPostSelfReportServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := postSelfReport(context.Background(), srv.URL, selfReport{Device: "remote:n"}); err == nil {
		t.Fatal("a 500 must surface as an error (the loop logs and retries)")
	}
}

func TestPostSelfReportUnreachable(t *testing.T) {
	// Nothing listens here: the error must come back quickly (the
	// request has its own timeout), not hang the node.
	start := time.Now()
	err := postSelfReport(context.Background(), "http://127.0.0.1:1", selfReport{Device: "remote:n"})
	if err == nil {
		t.Fatal("an unreachable resourced must error")
	}
	if time.Since(start) > 6*time.Second {
		t.Errorf("unreachable post took %s", time.Since(start))
	}
}

// TestLiveReadings: the snapshot always reports RAM (with or without a
// GPU on the machine); VRAM is 0 when there is none.
func TestLiveReadings(t *testing.T) {
	r, err := liveReadings(context.Background())
	if err != nil {
		t.Fatalf("liveReadings: %v", err)
	}
	if r.TotalRAMGB <= 0 {
		t.Errorf("TotalRAMGB = %v, want > 0", r.TotalRAMGB)
	}
	if r.FreeRAMGB < 0 || r.FreeRAMGB > r.TotalRAMGB {
		t.Errorf("FreeRAMGB = %v outside [0, %v]", r.FreeRAMGB, r.TotalRAMGB)
	}
	if r.TotalVRAMGB < 0 || (r.TotalVRAMGB > 0 && r.FreeVRAMGB > r.TotalVRAMGB) {
		t.Errorf("VRAM readings inconsistent: %+v", r)
	}
}
