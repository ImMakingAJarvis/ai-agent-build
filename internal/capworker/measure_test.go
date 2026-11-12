package capworker

// VRAM footprint measurement tests (EVB-28, AC2): the SDK measures a
// worker's actual nvidia-smi footprint on the first run and around
// every idle-reload Load, and reports it to the MeasurementReporter —
// with the declared catalog budget attached, so resourced can alert
// when measured > declared.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeMeasurer returns scripted used-MB readings in order (one per
// Measurer call); further calls repeat the last reading.
type fakeMeasurer struct {
	readings []int64
	err      error
	i        int
}

func (f *fakeMeasurer) measure(ctx context.Context) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	v := f.readings[f.i]
	if f.i+1 < len(f.readings) {
		f.i++
	}
	return v, nil
}

// fakeReporter records reported measurements on a channel —
// reportMeasurement is fire-and-forget, so the tests wait on it.
type fakeReporter struct {
	ch  chan Measurement
	err error
}

func (f *fakeReporter) ReportMeasurement(ctx context.Context, m Measurement) error {
	if f.err != nil {
		return f.err
	}
	f.ch <- m
	return nil
}

func expectReport(t *testing.T, ch chan Measurement) Measurement {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no measurement reported")
		return Measurement{}
	}
}

func expectNoReport(t *testing.T, ch chan Measurement) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("unexpected measurement %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestMeasureFirstRun is the AC2 demo path: a worker whose catalog
// budget is smaller than the model's actual footprint. The model comes
// up inside the first Handle, so the first-run measurement is the
// used-MB delta against the Run-start baseline — reported after the
// first successful Handle, once.
func TestMeasureFirstRun(t *testing.T) {
	t.Run("reports the first-run delta, over-declared", func(t *testing.T) {
		rep := &fakeReporter{ch: make(chan Measurement, 1)}
		w := New(Config{
			Cap: "image.describe", Name: "image-describe@gpu", Location: "gpu",
			LeaseSpec: LeaseSpec{VRAMMB: 3244}, // the catalog declares 3 GB
			Measurer:  (&fakeMeasurer{readings: []int64{23552}}).measure,
			MeasureReporter: rep,
		})
		w.baseVRAMMB.Store(16384) // the Run-start baseline read succeeded
		w.baseOK.Store(true)
		w.measureFirstRun()

		m := expectReport(t, rep.ch)
		if m.Holder != "image-describe@gpu" || m.Capability != "image.describe" || m.Device != "gpu" {
			t.Errorf("report identity = %+v", m)
		}
		if m.DeclaredMB != 3244 || m.MeasuredMB != 7168 {
			t.Errorf("declared/measured = %d/%d MB, want 3244/7168", m.DeclaredMB, m.MeasuredMB)
		}
		// The alert condition resourced checks: measured > declared.
		if m.MeasuredMB <= m.DeclaredMB {
			t.Error("the demo worker must measure over its declared budget")
		}
	})

	t.Run("measures once across later runs", func(t *testing.T) {
		rep := &fakeReporter{ch: make(chan Measurement, 2)}
		w := New(Config{
			Cap: "image.describe", Name: "image-describe@gpu", Location: "gpu",
			LeaseSpec:         LeaseSpec{VRAMMB: 3244},
			Measurer:          (&fakeMeasurer{readings: []int64{20000, 20000, 20000}}).measure,
			MeasureReporter: rep,
		})
		w.baseVRAMMB.Store(10000)
		w.baseOK.Store(true)
		w.measureFirstRun()
		expectReport(t, rep.ch)
		w.measureFirstRun()
		w.measureFirstRun()
		expectNoReport(t, rep.ch)
	})

	t.Run("skips when the baseline read failed", func(t *testing.T) {
		// No nvidia-smi in the container: Run's baseline read failed and
		// measurement turns itself off for the lifetime.
		rep := &fakeReporter{ch: make(chan Measurement, 1)}
		w := New(Config{
			Cap: "image.describe", Name: "image-describe@gpu", Location: "gpu",
			LeaseSpec:         LeaseSpec{VRAMMB: 3244},
			Measurer:          (&fakeMeasurer{readings: []int64{20000}}).measure,
			MeasureReporter: rep,
		})
		w.baseOK.Store(false)
		w.measureFirstRun()
		expectNoReport(t, rep.ch)
	})

	t.Run("skips a non-positive delta", func(t *testing.T) {
		// The worker started with its resources resident (or the GPU
		// usage moved down): the delta says nothing about the model.
		rep := &fakeReporter{ch: make(chan Measurement, 1)}
		w := New(Config{
			Cap: "image.describe", Name: "image-describe@gpu", Location: "gpu",
			LeaseSpec:         LeaseSpec{VRAMMB: 3244},
			Measurer:          (&fakeMeasurer{readings: []int64{16384}}).measure,
			MeasureReporter: rep,
		})
		w.baseVRAMMB.Store(20000)
		w.baseOK.Store(true)
		w.measureFirstRun()
		expectNoReport(t, rep.ch)
	})

	t.Run("skips when the after read fails", func(t *testing.T) {
		rep := &fakeReporter{ch: make(chan Measurement, 1)}
		w := New(Config{
			Cap: "image.describe", Name: "image-describe@gpu", Location: "gpu",
			LeaseSpec: LeaseSpec{VRAMMB: 3244},
			Measurer:  (&fakeMeasurer{err: errors.New("nvidia-smi: no such file")}).measure,
			MeasureReporter: rep,
		})
		w.baseVRAMMB.Store(16384)
		w.baseOK.Store(true)
		w.measureFirstRun()
		expectNoReport(t, rep.ch)
	})
}

// TestMeasureReload is the idle-reload path: ensureLoaded brackets the
// Load call with used-MB reads and reports the delta — a failed
// before-read or a non-positive delta just skips the measurement.
func TestMeasureReload(t *testing.T) {
	t.Run("the Load brackets its own measurement", func(t *testing.T) {
		rep := &fakeReporter{ch: make(chan Measurement, 1)}
		w := New(Config{
			Cap: "image.describe", Name: "image-describe@gpu", Location: "gpu",
			LeaseSpec:         LeaseSpec{VRAMMB: 3244},
			Measurer:          (&fakeMeasurer{readings: []int64{16384, 20480}}).measure,
			MeasureReporter: rep,
			Load:              func(ctx context.Context) error { return nil },
		})
		w.loaded = false
		if err := w.ensureLoaded(context.Background()); err != nil {
			t.Fatalf("ensureLoaded: %v", err)
		}
		m := expectReport(t, rep.ch)
		if m.MeasuredMB != 4096 {
			t.Errorf("reload delta = %d MB, want 4096", m.MeasuredMB)
		}
	})

	t.Run("a failed before-read skips the measurement", func(t *testing.T) {
		rep := &fakeReporter{ch: make(chan Measurement, 1)}
		w := New(Config{
			Cap: "image.describe", Name: "image-describe@gpu", Location: "gpu",
			Measurer:          (&fakeMeasurer{err: errors.New("nvidia-smi: timeout")}).measure,
			MeasureReporter: rep,
			Load:              func(ctx context.Context) error { return nil },
		})
		w.loaded = false
		if err := w.ensureLoaded(context.Background()); err != nil {
			t.Fatalf("ensureLoaded: %v", err)
		}
		expectNoReport(t, rep.ch)
	})

	t.Run("a non-positive delta skips the measurement", func(t *testing.T) {
		rep := &fakeReporter{ch: make(chan Measurement, 1)}
		w := New(Config{
			Cap: "image.describe", Name: "image-describe@gpu", Location: "gpu",
			Measurer:          (&fakeMeasurer{readings: []int64{20480, 16384}}).measure,
			MeasureReporter: rep,
			Load:              func(ctx context.Context) error { return nil },
		})
		w.loaded = false
		if err := w.ensureLoaded(context.Background()); err != nil {
			t.Fatalf("ensureLoaded: %v", err)
		}
		expectNoReport(t, rep.ch)
	})

	t.Run("a Load failure reports nothing", func(t *testing.T) {
		rep := &fakeReporter{ch: make(chan Measurement, 1)}
		w := New(Config{
			Cap: "image.describe", Name: "image-describe@gpu", Location: "gpu",
			Measurer:          (&fakeMeasurer{readings: []int64{16384}}).measure,
			MeasureReporter: rep,
			Load:              func(ctx context.Context) error { return errors.New("model load failed") },
		})
		w.loaded = false
		if err := w.ensureLoaded(context.Background()); err == nil {
			t.Fatal("ensureLoaded should surface the Load error")
		}
		expectNoReport(t, rep.ch)
	})
}

// brokerWithReporter is a LeaseBroker that also implements
// MeasurementReporter — ResourcedBroker is one in production, which is
// how the worker mains get the reporter without naming it.
type brokerWithReporter struct{ *fakeReporter }

func (brokerWithReporter) Request(ctx context.Context, spec LeaseSpec) (LeaseDecision, error) {
	return LeaseDecision{}, nil
}
func (brokerWithReporter) Release(ctx context.Context, leaseID string) error { return nil }
func (brokerWithReporter) State(ctx context.Context, leaseID string) (LeasePresence, error) {
	return LeasePresence(0), nil
}

// TestMeasureReporterResolution: the explicit MeasureReporter wins;
// without one, a LeaseBroker that implements MeasurementReporter is
// used; with neither, measurements are logged, not sent.
func TestMeasureReporterResolution(t *testing.T) {
	meas := func(ctx context.Context) (int64, error) { return 0, nil }
	broker := &brokerWithReporter{fakeReporter: &fakeReporter{ch: make(chan Measurement, 1)}}
	explicit := &fakeReporter{ch: make(chan Measurement, 1)}

	// The explicit MeasureReporter wins over the broker.
	w := New(Config{
		Cap: "x", Name: "x", Location: "gpu", Measurer: meas,
		LeaseBroker:     broker,
		MeasureReporter: explicit,
	})
	if w.measureReporter != explicit {
		t.Error("an explicit MeasureReporter should win over the broker")
	}
	w = New(Config{
		Cap: "x", Name: "x", Location: "gpu", Measurer: meas,
		LeaseBroker:     broker,
		MeasureReporter: explicit,
	})
	if w.measureReporter != explicit {
		t.Error("an explicit MeasureReporter should win over the broker")
	}
	w = New(Config{
		Cap: "x", Name: "x", Location: "gpu", Measurer: meas,
		LeaseBroker: broker,
	})
	if w.measureReporter != broker {
		t.Error("a broker that implements MeasurementReporter should be picked up")
	}
	w = New(Config{Cap: "x", Name: "x", Location: "gpu", Measurer: meas})
	if w.measureReporter != nil {
		t.Error("no reporter anywhere: measurements should be log-only")
	}
}
