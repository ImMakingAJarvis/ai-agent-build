package capworker

// VRAM footprint measurement (plan §3.6, EVB-28). The capability catalog
// declares the budget a placement requests as a lease; the first run of
// the capability measures what the model actually takes off the GPU
// (the nvidia-smi used-MB delta around the load) and reports it to
// resourced, which alerts when measured > declared and budgets future
// leases by the measured value.
//
// Two load paths are covered:
//
//   - the first run of the process lifetime. Some workers' Load hook is
//     a no-op because the SDK assumes the worker starts loaded and the
//     model actually comes up inside the first Handle (the VLM coprocess
//     workers do this). So the first-run measurement is the used-MB
//     delta against a baseline taken when Run started, reported after
//     the first successful Handle.
//   - every idle-reload: ensureLoaded brings the resources back after an
//     idle unload, and the measurement brackets that Load call.

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Measurement is one footprint report: what the worker declared (the
// catalog's lease budget) and what the GPU actually used while the model
// was in.
type Measurement struct {
	Holder     string // the worker identity (cap@device/worker)
	Capability string // e.g. "image.describe"
	Device     string // e.g. "gpu"
	DeclaredMB int64  // the lease budget the catalog declares
	MeasuredMB int64  // the nvidia-smi delta (MiB — the ledger's unit)
}

// Measurer reads the GPU's currently-used VRAM in MB (MiB, nvidia-smi's
// unit — the same unit the ledger and the catalog use).
type Measurer func(ctx context.Context) (int64, error)

// NewNvidiaSmiMeasurer returns a Measurer that reads the 4090's used
// VRAM via nvidia-smi (bin defaults to "nvidia-smi" when empty). The
// first line of the csv,noheader,nounits output is the number; the call
// is bounded (5 s) because it runs on the delivery path.
func NewNvidiaSmiMeasurer(bin string) Measurer {
	return func(ctx context.Context) (int64, error) {
		if bin == "" {
			bin = "nvidia-smi"
		}
		mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(mctx, bin,
			"--query-gpu=memory.used", "--format=csv,noheader,nounits").Output()
		if err != nil {
			return 0, fmt.Errorf("nvidia-smi: %w", err)
		}
		first := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		return strconv.ParseInt(first, 10, 64)
	}
}

// MeasurementReporter receives one VRAM footprint measurement from a
// worker. ResourcedBroker implements it (POST /measure); tests use a
// fake.
type MeasurementReporter interface {
	ReportMeasurement(ctx context.Context, m Measurement) error
}
