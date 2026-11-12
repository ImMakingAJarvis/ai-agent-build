// Package resources is the VRAM/RAM ledger and lease broker — the core
// of the resourced service (plan §3.6, "the maximize-limited-hardware
// part"). The ledger keeps every memory lease on the box: the brain's
// standing GPU lease (measured live, never hardcoded) plus the
// capability workers' granted leases. New requests are decided with the
// policy seed that this part of the build implements: grant if it fits
// (free = total − leases), queue if it does not but might later, deny
// with a reason if it never can (unknown device, bigger than the
// device, ...). The full policy — conversation first, game watchdog,
// batch windows, co-scheduling preferences — lands with the window
// controller (plan task 27), which reuses this ledger's types.
//
// The ledger is in-memory: the standing lease is re-measured on every
// startup, and broker leases are re-requested by their workers. Every
// decision is returned as an Outcome; the service turns Outcomes into
// lease.granted / lease.released / lease.denied events on the events
// exchange.
//
// Co-scheduling (plan §3.6 rule 5, EVB-28): free VRAM is the remainder
// (total − active leases), and any worker whose footprint fits it is
// granted without a swap window — a small LLM runs alongside a big image
// job on what is left. CPU and iGPU workers draw only the shared RAM
// pool and are grantable any time; their RAM requests are verified
// against the live free-RAM reading (SetLiveFreeRAM) in addition to the
// pool, so the ledger never commits more than the box actually has free
// right now.
//
// Game watchdog (plan §3.6 rule 2, EVB-29): while a game stream is
// active the ledger holds the GPU — SetStreamActive(true) plus
// RevokeGPULeases removes every capability worker's GPU lease (the
// brain's standing lease is exempt) and blocks new GPU grants until the
// stream ends; CPU and iGPU leases keep running. The service
// (cmd/resourced) runs the streamwatcher and drives both.
package resources

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Device names (plan §3.6). GPU has its own VRAM pool; CPU and IGPU
// both draw the shared system RAM pool. remote:<host> devices are
// registered by self-report when a remote worker comes online (EVB-37).
const (
	DeviceGPU    = "gpu"
	DeviceCPU    = "cpu"
	DeviceIGPU   = "igpu"
	RemotePrefix = "remote:"
)

// BrainHolder is the holder name of the brain's standing GPU lease.
const BrainHolder = "brain"

// Lease is one entry in the ledger (plan §3.6): lease{id, holder,
// device, vram_mb, ram_mb, preemptible, since}, plus the capability the
// lease serves (what a window controller needs to know what is waiting)
// and, for leases that waited, when they entered the queue.
type Lease struct {
	ID          string    `json:"id"`
	Holder      string    `json:"holder"`
	Capability  string    `json:"capability,omitempty"`
	Device      string    `json:"device"`
	VRAMMB      int64     `json:"vram_mb"`
	RAMMB       int64     `json:"ram_mb"`
	Preemptible bool      `json:"preemptible"`
	Since       time.Time `json:"since"`
	QueuedAt    time.Time `json:"queued_at,omitempty"` // set while queued; kept after a grant (wait-time record)
}

// Request is a POST /lease body.
type Request struct {
	Holder      string `json:"holder"`
	Capability  string `json:"capability,omitempty"`
	Device      string `json:"device"`
	VRAMMB      int64  `json:"vram_mb"`
	RAMMB       int64  `json:"ram_mb"`
	Preemptible bool   `json:"preemptible"`
}

// Status is the decision for a lease request.
type Status string

const (
	Granted Status = "granted"
	Queued  Status = "queued"
	Denied  Status = "denied"
)

// Outcome is the answer to one request. The status decides the lease.*
// event the service emits (granted/denied; a queued request emits none
// — it is visible in the queue until it is granted or withdrawn).
type Outcome struct {
	Status   Status `json:"status"`
	Lease    Lease  `json:"lease,omitempty"`
	Reason   string `json:"reason,omitempty"`   // denied: why it can never fit
	Position int    `json:"position,omitempty"` // queued: 1-based place in the FIFO
}

// Device is one of the box's resources with its live facts. VRAMTotalMB
// is set for the gpu (read live from nvidia-smi); RAMTotalMB for cpu
// and igpu (both report the same shared pool). Cores is informational
// (cpu only).
type Device struct {
	Name        string `json:"name"`
	Present     bool   `json:"present"`
	VRAMTotalMB int64  `json:"vram_total_mb"`
	RAMTotalMB  int64  `json:"ram_total_mb"`
	Cores       int    `json:"cores,omitempty"`
}

// State is the GET /state payload: the devices, the active leases
// (including the brain's standing lease), the wait queue and the
// game-stream hold (EVB-29: 1 while a game stream is active and the
// GPU queue is held).
type State struct {
	Devices      []Device `json:"devices"`
	Leases       []Lease  `json:"leases"`
	Queued       []Lease  `json:"queued"`
	StreamActive bool     `json:"stream_active"`
}

// Ledger is the in-memory lease ledger. All methods are safe for
// concurrent use.
type Ledger struct {
	mu      sync.Mutex
	devices map[string]Device // known device name → live facts
	active  map[string]Lease  // id → granted lease (incl. the brain standing lease)
	queued  []Lease           // FIFO wait queue
	seq     int64             // lease id counter
	// liveFreeRAMMB is the last live free-RAM reading in MB (the
	// /proc/meminfo MemAvailable the service folds in on each refresh).
	// 0 = no reading yet (or unreadable): RAM fits are judged on the pool
	// minus leases alone.
	liveFreeRAMMB int64
	// streamActive is the game-watchdog hold (plan §3.6 rule 2, EVB-29):
	// while a game stream is active the game's VRAM is not (yet) in the
	// ledger's free-remainder math, so GPU leases are revoked and no new
	// GPU lease may be granted — the stream owns the 4090. CPU and iGPU
	// leases are untouched (the rule is GPU-only; a game does not eat the
	// box's RAM pool).
	streamActive bool
}

// New builds an empty ledger over the given devices.
func New(devices ...Device) *Ledger {
	l := &Ledger{
		devices: make(map[string]Device, len(devices)),
		active:  make(map[string]Lease),
	}
	for _, d := range devices {
		l.devices[d.Name] = d
	}
	return l
}

// SetDevice registers or refreshes one device's live facts (capacity
// changes are picked up without a restart).
func (l *Ledger) SetDevice(d Device) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.devices[d.Name] = d
}

// SetLiveFreeRAM records the box's current free system RAM in MB (the
// live reading's MemAvailable, folded in by the service on each
// refresh). RAM requests — cpu and igpu leases, and a gpu lease's
// staging buffers — are granted only when they fit this in addition to
// the pool minus leases (plan §3.6 rule 5: RAM-only workers are
// grantable any time, verified against the live RAM). 0 = no reading
// yet: the pool check stands alone.
func (l *Ledger) SetLiveFreeRAM(mb int64) {
	if mb < 0 {
		mb = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.liveFreeRAMMB = mb
}

// Devices returns the known devices in stable (sorted) order.
func (l *Ledger) Devices() []Device {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Device, 0, len(l.devices))
	for _, d := range l.devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Request decides one lease request: granted when it fits in the free
// remainder (total − leases), queued when it does not but might later,
// denied with a reason when it never can.
//
// The VRAM fit is the rule-5 co-scheduling check: the remainder is
// total − every active GPU lease, so a request for whatever is left
// (a 5.5 GB LLM next to a 12 GB image job on a 24 GB card) is granted
// here, with no window. RAM requests are checked against the pool too —
// denied when bigger than the box, queued (not granted) until the live
// free-RAM reading (SetLiveFreeRAM) has room.
//
// Game stream (rule 2, EVB-29): while streamActive is set, a GPU
// request is always queued — never granted — because the stream's own
// VRAM is not in the remainder yet (it appears in the next measured
// refresh at the latest), and granting a worker into the headroom would
// put it in the game's way. CPU/iGPU requests (RAM only) go through the
// normal path: a game does not touch the RAM pool.
func (l *Ledger) Request(req Request) Outcome {
	l.mu.Lock()
	defer l.mu.Unlock()

	dev, known := l.devices[req.Device]
	switch {
	case !known && strings.HasPrefix(req.Device, RemotePrefix):
		return denied(Outcome{}, fmt.Sprintf("no self-report from %s (remote workers register in EVB-37)", req.Device))
	case !known:
		return denied(Outcome{}, fmt.Sprintf("unknown device %q (known: %s)", req.Device, l.knownDeviceNames()))
	case !dev.Present:
		return denied(Outcome{}, fmt.Sprintf("device %s is not present on this box", req.Device))
	case req.VRAMMB > 0 && dev.Name != DeviceGPU:
		return denied(Outcome{}, fmt.Sprintf("device %s has no VRAM", req.Device))
	case req.VRAMMB > dev.VRAMTotalMB:
		return denied(Outcome{}, fmt.Sprintf("needs %d MB VRAM, %s has %d MB total", req.VRAMMB, req.Device, dev.VRAMTotalMB))
	case req.RAMMB > l.ramPool():
		return denied(Outcome{}, fmt.Sprintf("needs %d MB RAM, the box has %d MB total", req.RAMMB, l.ramPool()))
	}

	// Rule 2 (EVB-29): the game stream owns the GPU — hold the request
	// in the queue until SetStreamActive(false) and the re-evaluation
	// that follows it.
	if req.VRAMMB > 0 && l.streamActive {
		lease := l.newLeaseLocked(req)
		lease.Since = time.Time{} // not granted yet; set on grant
		lease.QueuedAt = time.Now().UTC()
		l.queued = append(l.queued, lease)
		return Outcome{Status: Queued, Lease: lease, Position: len(l.queued)}
	}

	if req.VRAMMB <= l.freeVRAMLocked() && l.ramFitsLocked(req.RAMMB) {
		lease := l.newLeaseLocked(req)
		l.active[lease.ID] = lease
		return Outcome{Status: Granted, Lease: lease}
	}

	lease := l.newLeaseLocked(req)
	lease.Since = time.Time{} // not granted yet; set on grant
	lease.QueuedAt = time.Now().UTC()
	l.queued = append(l.queued, lease)
	return Outcome{Status: Queued, Lease: lease, Position: len(l.queued)}
}

// Release deletes the lease with id — whether granted or still queued
// (withdrawing a queued request) — and returns it. Unknown ids report
// ok=false. Callers follow up with Reevaluate: freeing memory can grant
// queued leases.
func (l *Ledger) Release(id string) (Lease, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lease, ok := l.active[id]; ok {
		delete(l.active, id)
		return lease, true
	}
	for i, lease := range l.queued {
		if lease.ID == id {
			l.queued = append(l.queued[:i], l.queued[i+1:]...)
			return lease, true
		}
	}
	return Lease{}, false
}

// Reevaluate grants queued leases that now fit, in FIFO order, and
// denies any that now never can fit (a capacity refresh shrank the
// device below the request). Returns one Outcome per lease moved, in
// the order it was moved — the service emits the matching lease.* event
// for each. Called after every release and after every live-reading
// refresh (the brain's standing lease changing size is the event that
// frees the GPU).
//
// Game stream (rule 2, EVB-29): while streamActive is set, queued GPU
// leases stay queued even when they would fit — the stream's VRAM is not
// in the remainder yet. Queued CPU/iGPU leases are unaffected.
func (l *Ledger) Reevaluate() []Outcome {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Outcome
	kept := l.queued[:0]
	for _, lease := range l.queued {
		dev := l.devices[lease.Device]
		switch {
		case lease.VRAMMB > dev.VRAMTotalMB || lease.RAMMB > l.ramPool():
			out = append(out, denied(Outcome{Lease: lease},
				fmt.Sprintf("no longer fits: needs %d MB VRAM / %d MB RAM, %s has %d MB VRAM / the box %d MB RAM",
					lease.VRAMMB, lease.RAMMB, dev.Name, dev.VRAMTotalMB, l.ramPool())))
		case lease.VRAMMB > 0 && l.streamActive:
			// The game stream owns the GPU — hold the GPU waiters in the
			// queue until the stream ends (Request keeps adding them,
			// this keeps them from jumping the hold).
			kept = append(kept, lease)
		case lease.VRAMMB <= l.freeVRAMLocked() && l.ramFitsLocked(lease.RAMMB):
			lease.Since = time.Now().UTC()
			l.active[lease.ID] = lease
			out = append(out, Outcome{Status: Granted, Lease: lease})
		default:
			kept = append(kept, lease)
		}
	}
	l.queued = kept
	return out
}

// RefreshBrainLease sets the brain's standing GPU lease to the measured
// GPU usage in MB (the live reading's total − free). The standing lease
// is created on the first call (reported as created=true, so the
// service emits the lease.granted) and resized in place afterwards —
// same id, same since, no further events.
//
// Measured usage, not a declared size: while the brain is the sole GPU
// tenant it is exactly the brain's footprint (~18–22 GB, whatever the
// loaded model needs); when the brain is out (swap window) it drops to
// driver overhead and the GPU frees up.
//
// Co-tenants (EVB-28): the service subtracts the active capability
// workers' declared GPU leases from the measured usage before calling —
// nvidia-smi's used counts the workers' models too, and their leases
// already reserve those bytes. Without the subtraction the ledger
// double-counts co-tenant usage and can never grant a small worker while
// a big one runs. The subtraction is clamped at 0 on the service side:
// when the declared budgets exceed the measured usage (workers not
// loaded yet), the standing lease floors and the ledger errs
// conservative — it never grants more than is really free.
func (l *Ledger) RefreshBrainLease(measuredVRAMMB int64) (Lease, bool) {
	if measuredVRAMMB < 0 {
		measuredVRAMMB = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, lease := range l.active {
		if lease.Holder == BrainHolder && lease.Device == DeviceGPU {
			lease.VRAMMB = measuredVRAMMB
			l.active[lease.ID] = lease
			return lease, false
		}
	}
	lease := Lease{
		ID:     l.nextIDLocked(),
		Holder: BrainHolder,
		Device: DeviceGPU,
		VRAMMB: measuredVRAMMB,
		Since:  time.Now().UTC(),
	}
	l.active[lease.ID] = lease
	return lease, true
}

// SetStreamActive turns the game-stream hold on or off (plan §3.6
// rule 2, EVB-29). On: the service revokes the capability workers' GPU
// leases (RevokeGPULeases) and no new GPU lease is granted until the
// stream ends — the game's VRAM is not in the ledger yet, so the
// remainder is not trustworthy for GPU grants. Off: the caller runs
// Reevaluate to grant the queued GPU leases that now fit (the stream's
// VRAM is gone from the box by then). CPU and iGPU leases — RAM only —
// are never touched by the hold (the rule is GPU-only).
func (l *Ledger) SetStreamActive(active bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.streamActive = active
}

// StreamActive reports whether the game-stream hold is on (GET /state).
func (l *Ledger) StreamActive() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.streamActive
}

// RevokeGPULeases is the game watchdog's revocation (plan §3.6 rule 2,
// EVB-29): it releases every GPU lease held by a capability worker —
// granted and queued — and returns them, in grant order, so the
// service can emit one lease.released event per revoked lease
// (invariant 10). The brain's standing lease is never touched (rules
// 1 and 3: a working brain is not evicted; the window controller brings
// the brain back through jarvis-swap, and the game waits for its VRAM
// after). CPU and iGPU leases stay — a game stream does not compete for
// system RAM.
func (l *Ledger) RevokeGPULeases() []Lease {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Lease
	for id, lease := range l.active {
		if lease.Device == DeviceGPU && lease.Holder != BrainHolder {
			out = append(out, lease)
			delete(l.active, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessID(out[i].ID, out[j].ID) })
	var kept []Lease
	for _, lease := range l.queued {
		if lease.Device == DeviceGPU && lease.Holder != BrainHolder {
			out = append(out, lease)
		} else {
			kept = append(kept, lease)
		}
	}
	l.queued = kept
	return out
}

// ActiveLeases returns the granted leases, including the brain's
// standing lease, in lease-id (grant) order.
func (l *Ledger) ActiveLeases() []Lease {
	return l.snapshotLocked(func(l *Ledger) []Lease {
		out := make([]Lease, 0, len(l.active))
		for _, lease := range l.active {
			out = append(out, lease)
		}
		sort.Slice(out, func(i, j int) bool { return lessID(out[i].ID, out[j].ID) })
		return out
	})
}

// Queued returns a copy of the wait queue in FIFO order.
func (l *Ledger) Queued() []Lease {
	return l.snapshotLocked(func(l *Ledger) []Lease {
		return append([]Lease{}, l.queued...)
	})
}

// State is a snapshot of the whole ledger (GET /state).
func (l *Ledger) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	leases := make([]Lease, 0, len(l.active))
	for _, lease := range l.active {
		leases = append(leases, lease)
	}
	sort.Slice(leases, func(i, j int) bool { return lessID(leases[i].ID, leases[j].ID) })
	return State{
		Devices:      l.devicesSortedLocked(),
		Leases:       leases,
		Queued:       append([]Lease{}, l.queued...),
		StreamActive: l.streamActive,
	}
}

// snapshotLocked runs f with the lock held, for the read-only snapshot
// methods that need several fields consistent.
func (l *Ledger) snapshotLocked(f func(*Ledger) []Lease) []Lease {
	l.mu.Lock()
	defer l.mu.Unlock()
	return f(l)
}

func (l *Ledger) newLeaseLocked(req Request) Lease {
	return Lease{
		ID:          l.nextIDLocked(),
		Holder:      req.Holder,
		Capability:  req.Capability,
		Device:      req.Device,
		VRAMMB:      req.VRAMMB,
		RAMMB:       req.RAMMB,
		Preemptible: req.Preemptible,
		Since:       time.Now().UTC(),
	}
}

func (l *Ledger) nextIDLocked() string {
	l.seq++
	return fmt.Sprintf("l%d", l.seq)
}

// ramPool is the shared system RAM pool in MB: cpu and igpu both draw
// it, and a gpu lease's ram_mb (host staging buffers) does too.
func (l *Ledger) ramPool() int64 {
	var pool int64
	for _, d := range l.devices {
		if d.Present && d.RAMTotalMB > pool {
			pool = d.RAMTotalMB
		}
	}
	return pool
}

// freeVRAMLocked is the free remainder of the GPU's VRAM pool: total −
// the vram_mb of every active GPU lease (the policy seed's rule 5).
func (l *Ledger) freeVRAMLocked() int64 {
	dev := l.devices[DeviceGPU]
	var used int64
	for _, lease := range l.active {
		if lease.Device == DeviceGPU {
			used += lease.VRAMMB
		}
	}
	return dev.VRAMTotalMB - used
}

// freeRAMLocked is the free remainder of the shared RAM pool: total −
// the ram_mb of every active lease, on any device.
func (l *Ledger) freeRAMLocked() int64 {
	var used int64
	for _, lease := range l.active {
		used += lease.RAMMB
	}
	return l.ramPool() - used
}

// ramFitsLocked is the grant test for a RAM request: it must fit the
// pool remainder AND the live free-RAM reading when one exists. The
// pool remainder counts committed leases; the live reading counts what
// the box actually has free right now (committed or not) — the min of
// the two is the conservative bound (rule 5).
func (l *Ledger) ramFitsLocked(ramMB int64) bool {
	if ramMB > l.freeRAMLocked() {
		return false
	}
	return l.liveFreeRAMMB == 0 || ramMB <= l.liveFreeRAMMB
}

func (l *Ledger) knownDeviceNames() string {
	names := make([]string, 0, len(l.devices))
	for name := range l.devices {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (l *Ledger) devicesSortedLocked() []Device {
	out := make([]Device, 0, len(l.devices))
	for _, d := range l.devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func denied(out Outcome, reason string) Outcome {
	out.Status = Denied
	out.Reason = reason
	return out
}

// lessID orders lease ids ("l<n>") numerically — grant order.
func lessID(a, b string) bool {
	na, oka := parseSeq(a)
	nb, okb := parseSeq(b)
	if oka && okb {
		return na < nb
	}
	return a < b
}

func parseSeq(id string) (int64, bool) {
	if len(id) < 2 || id[0] != 'l' {
		return 0, false
	}
	var n int64
	for _, c := range id[1:] {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}
