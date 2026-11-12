package resources

import (
	"strings"
	"testing"
)

// testLedger is the box of the plan: a 4090 with 24576 MB VRAM and 128 GB
// of shared system RAM (cpu and igpu both draw it).
func testLedger() *Ledger {
	return New(
		Device{Name: DeviceGPU, Present: true, VRAMTotalMB: 24576},
		Device{Name: DeviceCPU, Present: true, RAMTotalMB: 131072, Cores: 16},
		Device{Name: DeviceIGPU, Present: true, RAMTotalMB: 131072},
	)
}

func req(holder, device string, vram, ram int64) Request {
	return Request{Holder: holder, Capability: "test", Device: device, VRAMMB: vram, RAMMB: ram}
}

func status(t *testing.T, got, want Outcome) {
	t.Helper()
	if got.Status != want.Status {
		t.Fatalf("status = %q (reason %q), want %q", got.Status, got.Reason, want.Status)
	}
}

// TestRequestTable is the policy seed: grant if it fits (total − leases),
// queue if not but it might later, deny with a reason if never.
func TestRequestTable(t *testing.T) {
	tests := []struct {
		name   string
		req    Request
		want   Status
		reason string // substring of the denial reason, when denied
	}{
		{"grants: fits in the free VRAM remainder", req("w", DeviceGPU, 4096, 512), Granted, ""},
		{"grants: exactly the free remainder", req("w", DeviceGPU, 24576, 0), Granted, ""},
		{"grants: RAM-only lease on the cpu", req("w", DeviceCPU, 0, 4096), Granted, ""},
		{"grants: RAM-only lease on the igpu (shared pool)", req("w", DeviceIGPU, 0, 4096), Granted, ""},
		{"denies: RAM over the pool total", req("w", DeviceCPU, 0, 131073), Denied, "the box has 131072 MB total"},
		{"denies: unknown device", req("w", "tpu", 0, 1), Denied, "unknown device"},
		{"denies: unknown remote host before self-report", req("w", "remote:gpu-rack", 0, 1), Denied, "no self-report"},
		{"denies: VRAM bigger than the device total", req("w", DeviceGPU, 24577, 0), Denied, "has 24576 MB total"},
		{"denies: RAM bigger than the box total", req("w", DeviceCPU, 0, 200000), Denied, "the box has 131072 MB total"},
		{"denies: VRAM requested on a RAM-only device", req("w", DeviceCPU, 1024, 0), Denied, "has no VRAM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := testLedger().Request(tt.req)
			status(t, got, Outcome{Status: tt.want})
			if tt.want == Denied && tt.reason != "" && !strings.Contains(got.Reason, tt.reason) {
				t.Errorf("reason %q missing %q", got.Reason, tt.reason)
			}
			if tt.want == Queued && got.Position != 1 {
				t.Errorf("queue position = %d, want 1", got.Position)
			}
		})
	}
}

// TestCoScheduling is rule 5 (EVB-28): the free remainder is total −
// active leases, and a worker that fits it is granted without a swap
// window. A 12 GB image job on the 24 GB card leaves 12 GB — a small
// worker fits, a second big one does not, and two workers competing for
// the same remainder get one grant and one queue entry (FIFO).
func TestCoScheduling(t *testing.T) {
	l := testLedger()
	// The big job: image.generate's 12288 MB.
	if got := l.Request(req("image-gen", DeviceGPU, 12288, 0)); got.Status != Granted {
		t.Fatalf("image job: %v", got)
	}
	// Small fits the remainder: llm.small's 5500 MB next to the image job.
	if got := l.Request(req("llm-small", DeviceGPU, 5500, 0)); got.Status != Granted {
		t.Fatalf("small next to the big job = %q, want granted (no window)", got.Status)
	}
	// Big does not fit the remainder: 24576 − 12288 − 5500 = 6788 left,
	// a second 12288 job waits (it might fit once one of them releases).
	if got := l.Request(req("image-gen-2", DeviceGPU, 12288, 0)); got.Status != Queued {
		t.Fatalf("second big job = %q, want queued", got.Status)
	}
	// Two smalls compete for the remainder: clear the small and the
	// queued big job, and split the 12288 remainder between two 8192
	// workers — the first fits, the second must queue behind it.
	if _, ok := l.Release("l2"); !ok { // the 5500 MB small lease
		t.Fatal("releasing the small lease failed")
	}
	if _, ok := l.Release("l3"); !ok { // the queued second big job
		t.Fatal("withdrawing the queued big job failed")
	}
	if got := l.Request(req("small-a", DeviceGPU, 8192, 0)); got.Status != Granted {
		t.Fatalf("small-a: %v", got)
	}
	if got := l.Request(req("small-b", DeviceGPU, 8192, 0)); got.Status != Queued {
		t.Fatalf("small-b = %q, want queued (the remainder is 4096)", got.Status)
	}
	if got := l.Queued(); len(got) != 1 || got[0].Holder != "small-b" {
		t.Errorf("queue = %+v, want only small-b waiting", got)
	}
}

// TestRAMLiveGate is rule 5's RAM half: RAM-only workers (cpu, iGPU) are
// grantable any time, but their request must also fit the live free-RAM
// reading (SetLiveFreeRAM) — not the pool alone. Fitting the pool but
// not the live reading queues (the reading is a moment, it will change);
// bigger than the whole pool still denies.
func TestRAMLiveGate(t *testing.T) {
	l := testLedger()
	// No live reading yet: the pool check stands alone (the old behavior).
	if got := l.Request(req("a", DeviceCPU, 0, 4096)); got.Status != Granted {
		t.Fatalf("before the first reading: %v", got)
	}
	// The refresh says only 2 GB is free right now. A 4 GB RAM request
	// fits the pool (128 GB − 4 GB leased) but not the box — it queues.
	l.SetLiveFreeRAM(2048)
	if got := l.Request(req("b", DeviceCPU, 0, 4096)); got.Status != Queued {
		t.Fatalf("4 GB against 2 GB live free = %q, want queued", got.Status)
	}
	// A 1 GB request does fit the live reading.
	if got := l.Request(req("c", DeviceCPU, 0, 1024)); got.Status != Granted {
		t.Fatalf("1 GB against 2 GB live free = %q, want granted", got.Status)
	}
	// VRAM-only requests are not gated by RAM at all.
	if got := l.Request(req("d", DeviceGPU, 1024, 0)); got.Status != Granted {
		t.Fatalf("VRAM-only request with a tight live RAM: %v", got)
	}
	// The box frees up: the queued 4 GB now fits the live reading too.
	l.SetLiveFreeRAM(8192)
	outs := l.Reevaluate()
	if len(outs) != 1 || outs[0].Status != Granted || outs[0].Lease.Holder != "b" {
		t.Fatalf("reevaluate after RAM freed = %+v, want b granted", outs)
	}
	// Bigger than the whole pool still denies, whatever the reading says.
	l.SetLiveFreeRAM(1 << 40)
	if got := l.Request(req("e", DeviceCPU, 0, 200000)); got.Status != Denied {
		t.Fatalf("200 GB request: %v, want denied", got)
	}
	// A negative reading clamps to 0 = "no reading", the pool stands alone.
	l.SetLiveFreeRAM(-1)
	if got := l.Request(req("f", DeviceCPU, 0, 1024)); got.Status != Granted {
		t.Fatalf("after a clamped reading: %v", got)
	}
}

// TestAbsentDevice: a probe that failed (the GPU is off) denies instead
// of guessing at capacity.
func TestAbsentDevice(t *testing.T) {
	l := New(Device{Name: DeviceGPU, Present: false})
	got := l.Request(req("w", DeviceGPU, 1024, 0))
	status(t, got, Outcome{Status: Denied})
	if !strings.Contains(got.Reason, "not present") {
		t.Errorf("reason %q missing 'not present'", got.Reason)
	}
}

// TestSharedRAMPool: cpu, igpu and gpu leases all draw the same system
// RAM pool, so the remainder is checked across devices.
func TestSharedRAMPool(t *testing.T) {
	l := testLedger()
	// 64 GB on the cpu, 64 GB on the igpu: the pool (128 GB) is full.
	if got := l.Request(req("a", DeviceCPU, 0, 65536)); got.Status != Granted {
		t.Fatalf("cpu lease: %v", got)
	}
	if got := l.Request(req("b", DeviceIGPU, 0, 65536)); got.Status != Granted {
		t.Fatalf("igpu lease: %v", got)
	}
	// The third one does not fit — but it might, so it queues, not denies.
	if got := l.Request(req("c", DeviceCPU, 0, 1024)); got.Status != Queued {
		t.Fatalf("third lease = %q, want queued", got.Status)
	}
}

// TestReleaseReevaluate: releasing a lease frees the remainder, and the
// FIFO queue is re-evaluated — the earliest waiting lease that now fits
// is the one granted, and a swap window (the brain's standing lease
// shrinking to zero) does the same.
func TestReleaseReevaluate(t *testing.T) {
	l := testLedger()
	brain := l.Request(req(BrainHolder, DeviceGPU, 20480, 0))
	worker := l.Request(req("worker", DeviceGPU, 2048, 0))
	if brain.Status != Granted || worker.Status != Granted {
		t.Fatalf("setup: %v / %v", brain.Status, worker.Status)
	}
	// 2048 MB free: both requests are bigger, so both queue.
	first := l.Request(req("first", DeviceGPU, 3072, 0))
	second := l.Request(req("second", DeviceGPU, 2560, 0))
	if first.Status != Queued || second.Status != Queued {
		t.Fatalf("queue setup: %v / %v", first.Status, second.Status)
	}
	if second.Position != 2 {
		t.Errorf("second position = %d, want 2", second.Position)
	}
	if outs := l.Reevaluate(); len(outs) != 0 {
		t.Fatalf("reevaluate with 2048 MB free moved %d leases, want 0", len(outs))
	}
	// The worker's job finishes: 4096 MB free. Only the FIFO-first
	// (3072) fits; the second (2560) stays behind it.
	if _, ok := l.Release(worker.Lease.ID); !ok {
		t.Fatal("releasing the worker lease failed")
	}
	outs := l.Reevaluate()
	if len(outs) != 1 || outs[0].Status != Granted || outs[0].Lease.ID != first.Lease.ID {
		t.Fatalf("reevaluate = %+v, want the FIFO-first lease granted", outs)
	}
	if len(l.Queued()) != 1 || l.Queued()[0].ID != second.Lease.ID {
		t.Errorf("queue after the first grant = %+v, want the second waiting", l.Queued())
	}
	// The swap window opens: the brain's standing lease measures to
	// zero, and the last waiter fits in what is left.
	l.RefreshBrainLease(0)
	outs = l.Reevaluate()
	if len(outs) != 1 || outs[0].Status != Granted || outs[0].Lease.ID != second.Lease.ID {
		t.Fatalf("reevaluate in the window = %+v, want the FIFO-second lease granted", outs)
	}
	if len(l.Queued()) != 0 {
		t.Error("the queue should be empty once both fit")
	}
	// The brain is back: the queue re-evaluates to nothing (it is empty,
	// and a shrinking remainder never re-takes granted leases).
	l.RefreshBrainLease(20480)
	if outs = l.Reevaluate(); len(outs) != 0 {
		t.Fatalf("reevaluate with the brain back moved %d leases, want 0", len(outs))
	}
}

// TestReleaseWithdrawsQueued: DELETE on a queued lease withdraws it.
func TestReleaseWithdrawsQueued(t *testing.T) {
	l := testLedger()
	if got := l.Request(req(BrainHolder, DeviceGPU, 19456, 0)); got.Status != Granted {
		t.Fatalf("brain setup: %v", got)
	}
	queued := l.Request(req("w", DeviceGPU, 20480, 0))
	if queued.Status != Queued {
		t.Fatalf("setup: %v", queued)
	}
	if lease, ok := l.Release(queued.Lease.ID); !ok || lease.Holder != "w" {
		t.Fatalf("withdrawing the queued lease: ok=%v lease=%+v", ok, lease)
	}
	if len(l.Queued()) != 0 {
		t.Error("queue should be empty after the withdrawal")
	}
	if _, ok := l.Release(queued.Lease.ID); ok {
		t.Error("releasing the same id twice should report not found")
	}
}

// TestBrainStandingLease: the brain's lease is measured, created on the
// first refresh (created=true) and resized in place afterwards (same
// id, no second lease, created=false).
func TestBrainStandingLease(t *testing.T) {
	l := testLedger()
	lease, created := l.RefreshBrainLease(19456)
	if !created {
		t.Fatal("first refresh should create the standing lease")
	}
	if lease.Holder != BrainHolder || lease.Device != DeviceGPU || lease.VRAMMB != 19456 {
		t.Errorf("standing lease = %+v", lease)
	}
	// It counts against the free remainder like any other lease.
	if got := l.Request(req("w", DeviceGPU, 24576, 0)); got.Status != Queued {
		t.Errorf("full-GPU request with the brain in = %q, want queued", got.Status)
	}
	// The conversation deepens: the KV cache grows, the lease is resized
	// in place.
	resized, created := l.RefreshBrainLease(21504)
	if created {
		t.Error("second refresh should not create a second standing lease")
	}
	if resized.ID != lease.ID {
		t.Errorf("resize changed the lease id: %s -> %s", lease.ID, resized.ID)
	}
	if got := len(l.ActiveLeases()); got != 1 {
		t.Errorf("active leases = %d, want 1 (the standing lease)", got)
	}
	// Negative readings clamp to zero (the GPU is empty in the window).
	l.RefreshBrainLease(-5)
	if leases := l.ActiveLeases(); len(leases) != 1 || leases[0].VRAMMB != 0 {
		t.Errorf("after clamp: %+v", leases)
	}
}

// TestReevaluateDeniesShrunkDevice: a capacity refresh that shrinks the
// device below a queued request denies it with a reason.
func TestReevaluateDeniesShrunkDevice(t *testing.T) {
	l := New(Device{Name: DeviceGPU, Present: true, VRAMTotalMB: 24576})
	if got := l.Request(req(BrainHolder, DeviceGPU, 8192, 0)); got.Status != Granted {
		t.Fatalf("brain setup: %v", got)
	}
	queued := l.Request(req("w", DeviceGPU, 20000, 0))
	if queued.Status != Queued {
		t.Fatalf("setup: %v", queued)
	}
	// A swap-out box with less VRAM than was probed at start.
	l.SetDevice(Device{Name: DeviceGPU, Present: true, VRAMTotalMB: 16384})
	outs := l.Reevaluate()
	if len(outs) != 1 || outs[0].Status != Denied {
		t.Fatalf("reevaluate = %+v, want one denial", outs)
	}
	if !strings.Contains(outs[0].Reason, "no longer fits") {
		t.Errorf("reason %q missing 'no longer fits'", outs[0].Reason)
	}
	if len(l.Queued()) != 0 {
		t.Error("the denied lease should leave the queue")
	}
}

// TestStateSnapshot: GET /state shows devices, the active leases
// (including the brain's) and the queue in FIFO order.
func TestStateSnapshot(t *testing.T) {
	l := testLedger()
	l.RefreshBrainLease(10240)
	queued := l.Request(req("w", DeviceGPU, 20480, 0))
	if queued.Status != Queued {
		t.Fatalf("setup: %v", queued)
	}
	st := l.State()
	if len(st.Devices) != 3 {
		t.Errorf("devices = %d, want 3", len(st.Devices))
	}
	if len(st.Leases) != 1 || st.Leases[0].Holder != BrainHolder {
		t.Errorf("active leases = %+v, want the brain standing lease", st.Leases)
	}
	if len(st.Queued) != 1 || st.Queued[0].ID != queued.Lease.ID {
		t.Errorf("queue = %+v, want the queued request", st.Queued)
	}
}

// TestStreamHold is rule 2 (EVB-29): while a game stream is active the
// stream's VRAM is not in the ledger's remainder yet, so GPU requests
// force-queue (even when they would fit) and queued GPU waiters stay
// held — but the hold is GPU-only: RAM waiters still grant and RAM
// requests still pass.
func TestStreamHold(t *testing.T) {
	l := testLedger()
	l.RefreshBrainLease(18432) // the idle brain: 18.5 GB of the 24 GB card
	if got := l.Request(req("w", DeviceGPU, 4096, 0)); got.Status != Granted {
		t.Fatalf("before the hold: %v", got)
	}
	// A RAM waiter held by the live gate (it fits the pool, not the box).
	l.SetLiveFreeRAM(4096)
	if got := l.Request(req("cpu-w", DeviceCPU, 0, 8192)); got.Status != Queued {
		t.Fatalf("RAM waiter setup: %v", got)
	}
	// The stream starts: the hold is on.
	l.SetStreamActive(true)
	if !l.StreamActive() {
		t.Fatal("StreamActive = false, want true")
	}
	// A GPU request that fits the declared remainder (2048 MB left)
	// queues anyway — the stream's own VRAM is not in the math yet.
	if got := l.Request(req("w2", DeviceGPU, 2048, 0)); got.Status != Queued || got.Position != 2 {
		t.Fatalf("GPU request under the hold = %+v, want queued at 2", got)
	}
	// Reevaluate holds the GPU waiter; the RAM waiter is still gated by
	// the live reading, so nothing moves.
	if outs := l.Reevaluate(); len(outs) != 0 {
		t.Fatalf("reevaluate under the hold moved %d leases, want 0: %+v", len(outs), outs)
	}
	// The box frees RAM AND the stream ends: both waiters grant.
	l.SetLiveFreeRAM(16384)
	l.SetStreamActive(false)
	if l.StreamActive() {
		t.Fatal("StreamActive = true after the stream ended")
	}
	outs := l.Reevaluate()
	if len(outs) != 2 {
		t.Fatalf("reevaluate after the hold = %+v, want both waiters granted", outs)
	}
	if outs[0].Status != Granted || outs[0].Lease.Holder != "cpu-w" ||
		outs[1].Status != Granted || outs[1].Lease.Holder != "w2" {
		t.Errorf("grant order = %+v, want cpu-w then w2 (FIFO)", outs)
	}
}

// TestRevokeGPULeases is the game watchdog's revocation: every capability
// worker's GPU lease — granted AND queued — leaves the ledger; the brain's
// standing lease and every CPU/iGPU lease (granted or queued) stay. The
// returned list is id-ordered and is what the caller emits lease.released
// events for; a second call finds nothing (idempotent).
func TestRevokeGPULeases(t *testing.T) {
	l := testLedger()
	if _, created := l.RefreshBrainLease(18432); !created {
		t.Fatal("brain setup: no standing lease")
	}
	if got := l.Request(req("w-a", DeviceGPU, 2048, 0)); got.Status != Granted {
		t.Fatalf("w-a: %v", got)
	}
	if got := l.Request(req("w-b", DeviceGPU, 2048, 0)); got.Status != Granted {
		t.Fatalf("w-b: %v", got)
	}
	if got := l.Request(req("w-cpu", DeviceCPU, 0, 4096)); got.Status != Granted {
		t.Fatalf("w-cpu: %v", got)
	}
	if got := l.Request(req("w-igpu", DeviceIGPU, 0, 4096)); got.Status != Granted {
		t.Fatalf("w-igpu: %v", got)
	}
	l.SetLiveFreeRAM(1024)
	if got := l.Request(req("w-q-ram", DeviceCPU, 0, 8192)); got.Status != Queued {
		t.Fatalf("w-q-ram: %v", got)
	}
	if got := l.Request(req("w-q-gpu", DeviceGPU, 12288, 0)); got.Status != Queued {
		t.Fatalf("w-q-gpu: %v", got)
	}
	revoked := l.RevokeGPULeases()
	if len(revoked) != 3 {
		t.Fatalf("revoked = %+v, want the two granted GPU leases and the queued one", revoked)
	}
	if revoked[0].Holder != "w-a" || revoked[1].Holder != "w-b" || revoked[2].Holder != "w-q-gpu" {
		t.Errorf("revoked order = %+v, want w-a, w-b, w-q-gpu (id order)", revoked)
	}
	active := l.ActiveLeases()
	if len(active) != 3 {
		t.Fatalf("active after revoke = %+v, want the brain, cpu and igpu leases", active)
	}
	brain := active[0]
	if brain.Holder != BrainHolder || brain.VRAMMB != 18432 {
		t.Errorf("the brain standing lease was touched: %+v", brain)
	}
	if queued := l.Queued(); len(queued) != 1 || queued[0].Holder != "w-q-ram" {
		t.Errorf("queue after revoke = %+v, want only the RAM waiter", queued)
	}
	if again := l.RevokeGPULeases(); len(again) != 0 {
		t.Errorf("second revoke = %+v, want empty (idempotent)", again)
	}
}

// TestGrantOrdering: leases list in grant (id) order, l10 before l2.
func TestGrantOrdering(t *testing.T) {
	l := testLedger()
	for i := 0; i < 12; i++ {
		if got := l.Request(req("w", DeviceCPU, 0, 64)); got.Status != Granted {
			t.Fatalf("lease %d: %v", i, got)
		}
	}
	leases := l.ActiveLeases()
	if len(leases) != 12 {
		t.Fatalf("leases = %d, want 12", len(leases))
	}
	if leases[0].ID != "l1" || leases[10].ID != "l11" || leases[11].ID != "l12" {
		t.Errorf("grant order broken: first=%s tenth=%s last=%s", leases[0].ID, leases[10].ID, leases[11].ID)
	}
}
