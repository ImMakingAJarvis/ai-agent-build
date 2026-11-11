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
// (including the brain's standing lease) and the wait queue.
type State struct {
	Devices []Device `json:"devices"`
	Leases  []Lease  `json:"leases"`
	Queued  []Lease  `json:"queued"`
}

// Ledger is the in-memory lease ledger. All methods are safe for
// concurrent use.
type Ledger struct {
	mu      sync.Mutex
	devices map[string]Device // known device name → live facts
	active  map[string]Lease  // id → granted lease (incl. the brain standing lease)
	queued  []Lease           // FIFO wait queue
	seq     int64             // lease id counter
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

	if req.VRAMMB <= l.freeVRAMLocked() && req.RAMMB <= l.freeRAMLocked() {
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
		case lease.VRAMMB <= l.freeVRAMLocked() && lease.RAMMB <= l.freeRAMLocked():
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
// driver overhead and the GPU frees up; with co-tenants it counts their
// usage too, so the ledger errs conservative — it never grants more
// than is really free.
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
		Devices: l.devicesSortedLocked(),
		Leases:  leases,
		Queued:  append([]Lease{}, l.queued...),
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
