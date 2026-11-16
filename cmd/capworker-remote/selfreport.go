package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/fit"
)

// selfReport is the payload the node posts to resourced's
// POST /device-report (EVB-37): the device id remote:<node> plus the
// node's own memory capacities, so resourced can account for it.
// Read-only: the remote reports itself and never takes leases from the
// box (resourced denies lease requests for remote: devices).
type selfReport struct {
	Device      string `json:"device"`
	VRAMTotalMB int64  `json:"vram_total_mb"`
	RAMTotalMB  int64  `json:"ram_total_mb"`
	RAMFreeMB   int64  `json:"ram_free_mb"`
}

// buildSelfReport maps a live reading to the report payload. Readings
// are GiB; the report is MB (resourced's ledger unit).
func buildSelfReport(node string, r fit.Readings) selfReport {
	return selfReport{
		Device:      "remote:" + node,
		VRAMTotalMB: int64(r.TotalVRAMGB * 1024),
		RAMTotalMB:  int64(r.TotalRAMGB * 1024),
		RAMFreeMB:   int64(r.FreeRAMGB * 1024),
	}
}

// liveReadings takes the node's memory snapshot: the full reading when
// nvidia-smi is present, otherwise RAM only (VRAM 0 — the node has no
// GPU, and resourced records that as "no VRAM").
func liveReadings(ctx context.Context) (fit.Readings, error) {
	l := fit.Live{}
	r, err := l.Readings(ctx)
	if err == nil {
		return r, nil
	}
	free, total, rerr := l.RAM()
	if rerr != nil {
		return fit.Readings{}, fmt.Errorf("live readings: %w (ram fallback: %v)", err, rerr)
	}
	return fit.Readings{FreeRAMGB: free, TotalRAMGB: total}, nil
}

// postSelfReport delivers one report to resourced. The caller treats it
// as best-effort: an error is logged and retried on the next tick.
func postSelfReport(ctx context.Context, baseURL string, rep selfReport) error {
	body, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, baseURL+"/device-report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("resourced: %s", resp.Status)
	}
	return nil
}

// runSelfReport posts one report immediately, then every interval,
// until ctx ends. A failure (resourced down, no memory readable) is
// logged and retried on the next tick — resourced is an observation
// point, not a dependency of the worker.
func runSelfReport(ctx context.Context, node, baseURL string, interval time.Duration, log *log.Logger) {
	reportOnce := func() {
		r, err := liveReadings(ctx)
		if err != nil {
			log.Printf("capworker-remote: self-report: %v", err)
			return
		}
		rep := buildSelfReport(node, r)
		if err := postSelfReport(ctx, baseURL, rep); err != nil {
			log.Printf("capworker-remote: self-report to %s: %v", baseURL, err)
			return
		}
		log.Printf("capworker-remote: self-reported %s (ram free/total %d/%d MB, vram total %d MB)",
			rep.Device, rep.RAMFreeMB, rep.RAMTotalMB, rep.VRAMTotalMB)
	}
	reportOnce()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reportOnce()
		}
	}
}
