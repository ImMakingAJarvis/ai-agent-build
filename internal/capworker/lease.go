// The resourced lease integration (plan §3.6, task EVB-26): the lease
// decides who may consume. A worker requests a lease from resourced
// (task EVB-25) before every consume session — holder = the worker's
// identity, capability, device from its location, memory from the
// capability catalog. No lease → the worker does not consume, and the
// broker's consumer priorities deliver the queue to a lower-priority
// placement of the same capability (the fallback emerges from priorities
// + lease cancellation — there is no router). A lease granted while the
// worker waits → it re-consumes; a lease lost (revoked by the window
// controller, task 29) → the in-flight delivery drains, then the
// consumer cancels.
//
// Fail-open (documented behavior): when resourced is unreachable the
// worker assumes granted and keeps the pre-26 behavior — the ledger is
// an optimization for co-scheduling, not a hard dependency, and a
// resourced outage must not idle every capability worker on the box.
// The worker re-syncs on its poll tick once resourced answers again (a
// re-request then decides for real: granted, queued or denied).
package capworker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/catalog"
)

// Lease request answers (resourced's POST /lease statuses).
const (
	LeaseGranted = "granted"
	LeaseQueued  = "queued"
	LeaseDenied  = "denied"
)

// LeaseSpec is one worker's lease request: who (holder), what
// (capability), where (device) and what it will hold (vram_mb / ram_mb,
// from the capability catalog's entry for the placement).
type LeaseSpec struct {
	Holder     string
	Capability string
	Device     string
	VRAMMB     int64
	RAMMB      int64
}

// LeaseDecision is the broker's answer to one request.
type LeaseDecision struct {
	Status   string // LeaseGranted | LeaseQueued | LeaseDenied
	LeaseID  string // granted or queued: the lease id to track
	Reason   string // denied: why it can never fit
	Position int    // queued: 1-based place in the FIFO
}

// LeasePresence is what a State lookup reports about one lease id.
type LeasePresence int

const (
	LeaseGone LeasePresence = iota // not active, not queued: released, denied, or a resourced restart (the ledger is in-memory)
	LeaseInQueue                   // still waiting in the FIFO
	LeaseActive                    // granted
)

func (p LeasePresence) String() string {
	switch p {
	case LeaseInQueue:
		return "in the queue"
	case LeaseActive:
		return "active"
	default:
		return "gone"
	}
}

// LeaseBroker is the lease API a worker gates on. The production
// implementation is ResourcedBroker (task EVB-25's HTTP API); tests use
// a fake.
type LeaseBroker interface {
	// Request asks for the lease (POST /lease). The three statuses are
	// normal answers, not errors; an error means the broker is
	// unreachable.
	Request(ctx context.Context, spec LeaseSpec) (LeaseDecision, error)
	// Release gives a lease back (DELETE /lease/<id>); an already-gone
	// lease is a no-op.
	Release(ctx context.Context, leaseID string) error
	// State reports where a lease stands (GET /state).
	State(ctx context.Context, leaseID string) (LeasePresence, error)
}

// ResourcedBroker is the HTTP implementation of LeaseBroker, talking to
// a running resourced.
type ResourcedBroker struct {
	BaseURL string // e.g. http://resourced:9104 (compose net)
	Client  *http.Client
}

// NewResourcedBroker builds a broker client for a resourced at baseURL.
func NewResourcedBroker(baseURL string) *ResourcedBroker {
	return &ResourcedBroker{
		BaseURL: baseURL,
		Client:  &http.Client{Timeout: 5 * time.Second},
	}
}

// Request asks for the lease (POST /lease). 201/202/409 are all valid
// answers (granted/queued/denied) — only a transport failure or another
// status is an error.
func (b *ResourcedBroker) Request(ctx context.Context, spec LeaseSpec) (LeaseDecision, error) {
	var resp struct {
		Status string `json:"status"`
		Lease  struct {
			ID string `json:"id"`
		} `json:"lease"`
		Reason   string `json:"reason"`
		Position int    `json:"position"`
	}
	err := b.doJSON(ctx, http.MethodPost, "/lease", map[string]any{
		"holder":      spec.Holder,
		"capability":  spec.Capability,
		"device":      spec.Device,
		"vram_mb":     spec.VRAMMB,
		"ram_mb":      spec.RAMMB,
	}, &resp, http.StatusCreated, http.StatusAccepted, http.StatusConflict)
	if err != nil {
		return LeaseDecision{}, err
	}
	return LeaseDecision{Status: resp.Status, LeaseID: resp.Lease.ID, Reason: resp.Reason, Position: resp.Position}, nil
}

// Release gives a lease back (DELETE /lease/<id>). A 404 — resourced
// already released it (a revocation, or it restarted and its in-memory
// ledger is fresh) — is a successful no-op.
func (b *ResourcedBroker) Release(ctx context.Context, leaseID string) error {
	return b.doJSON(ctx, http.MethodDelete, "/lease/"+leaseID, nil, nil,
		http.StatusOK, http.StatusNotFound)
}

// State reports where a lease stands (GET /state, scanned for the id):
// active when granted, in the queue while waiting, gone otherwise.
func (b *ResourcedBroker) State(ctx context.Context, leaseID string) (LeasePresence, error) {
	var st struct {
		Leases []struct {
			ID string `json:"id"`
		} `json:"leases"`
		Queued []struct {
			ID string `json:"id"`
		} `json:"queued"`
	}
	if err := b.doJSON(ctx, http.MethodGet, "/state", nil, &st, http.StatusOK); err != nil {
		return LeaseGone, err
	}
	for _, l := range st.Leases {
		if l.ID == leaseID {
			return LeaseActive, nil
		}
	}
	for _, q := range st.Queued {
		if q.ID == leaseID {
			return LeaseInQueue, nil
		}
	}
	return LeaseGone, nil
}

// doJSON runs one request against the broker. okStatuses are the
// expected answers (a 409 deny is an answer, not a failure); anything
// else — or a transport error — is returned for the caller to treat as
// "broker unreachable" (the fail-open path).
func (b *ResourcedBroker) doJSON(ctx context.Context, method, path string, body, out any, okStatuses ...int) error {
	var rd io.Reader
	if body != nil {
		bb, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(bb)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := b.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for _, s := range okStatuses {
		if resp.StatusCode == s {
			if out != nil {
				return json.NewDecoder(resp.Body).Decode(out)
			}
			return nil
		}
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return fmt.Errorf("resourced: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
}

// LeaseFor wires the lease gating for one worker placement (plan §3.6):
// a resourced broker at baseURL and the spec of the placement's
// capability-catalog entry (location, memory footprints). It returns no
// broker when baseURL is empty (deliberately un-gated — tests and
// machines without resourced), or on any catalog problem (the error is
// for the worker to log and run un-gated on): a worker without a spec
// cannot say what it would hold, and the pre-26 behavior is the safe
// fallback.
func LeaseFor(baseURL, catalogPath, cap, location, holder string) (LeaseBroker, LeaseSpec, error) {
	if baseURL == "" {
		return nil, LeaseSpec{}, nil
	}
	cat, err := catalog.Load(catalogPath)
	if err != nil {
		return nil, LeaseSpec{}, fmt.Errorf("catalog %s: %w", catalogPath, err)
	}
	c, ok := cat.Capabilities[cap]
	if !ok {
		return nil, LeaseSpec{}, fmt.Errorf("catalog %s: no capability %q", catalogPath, cap)
	}
	w := c.WorkerAt(location)
	if w == nil {
		return nil, LeaseSpec{}, fmt.Errorf("catalog %s: capability %q has no %s placement", catalogPath, cap, location)
	}
	return NewResourcedBroker(baseURL), LeaseSpec{
		Holder:     holder,
		Capability: cap,
		Device:     location,
		VRAMMB:     int64(w.VRAMMB),
		RAMMB:      int64(w.RAMMB),
	}, nil
}

// ---------------- the state machine ----------------

// leaseState is where the worker stands vis-à-vis its lease.
type leaseState int

const (
	leaseNone     leaseState = iota // no lease: never asked, denied, lost, or released
	leaseQueued                     // the request is in resourced's FIFO
	leaseHeld                       // a lease is granted
	leaseFailOpen                   // resourced unreachable: assume granted
)

// leaseManager owns one worker's lease: it requests before a consume
// session, tracks the lease (queued → granted, held → revoked), releases
// it on idle unload and shutdown, and tells the consume loop when the
// lease is lost (drain the in-flight delivery, then cancel).
type leaseManager struct {
	broker LeaseBroker
	spec   LeaseSpec
	log    *log.Logger
	poll   time.Duration // re-ask / re-check interval

	mu      sync.Mutex
	state   leaseState
	leaseID string
	reason  string // last deny reason (for logs)
	askedAt time.Time // last request to the broker (rate limit)
	lostCh  chan struct{} // closed when a held lease is lost
}

func newLeaseManager(broker LeaseBroker, spec LeaseSpec, poll time.Duration, log *log.Logger) *leaseManager {
	if poll < time.Second {
		poll = time.Second
	}
	return &leaseManager{
		broker: broker,
		spec:   spec,
		log:    log,
		poll:   poll,
		lostCh: make(chan struct{}),
	}
}

// holding reports whether the worker may consume: it holds a granted
// lease, or resourced is unreachable and the worker fails open.
func (lm *leaseManager) holding() bool {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.state == leaseHeld || lm.state == leaseFailOpen
}

// ensureLease moves the state machine forward (rate-limited to one
// request per poll interval) and reports whether the worker may
// consume. It is called at the start of every consume session, so the
// first request happens immediately at startup.
func (lm *leaseManager) ensureLease(ctx context.Context) bool {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.advanceLocked(ctx)
	return lm.state == leaseHeld || lm.state == leaseFailOpen
}

// tick runs one periodic advance (the Run loop's goroutine): a denied or
// lost lease is re-asked, a queued one checked for its grant, a held one
// for a revocation (the window controller's task-29 path), and a
// fail-open one re-requested so the decision becomes real when
// resourced comes back.
func (lm *leaseManager) tick(ctx context.Context) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.advanceLocked(ctx)
}

// lost returns the channel that closes when the currently held lease is
// lost (revoked, or a fail-open re-sync that comes back denied). The
// consume loop selects on it: drain, then cancel the consumer.
func (lm *leaseManager) lost() <-chan struct{} {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.lostCh
}

// releaseHeld gives a held lease back (the idle-unload path: the
// resources are free, so the ledger's remainder is real again). A queued
// request is kept — the worker still wants to run when the resource
// frees. The next re-ask is deferred by a full poll interval: an
// idle-unloaded worker must not bounce straight back to consuming.
func (lm *leaseManager) releaseHeld(ctx context.Context) {
	lm.mu.Lock()
	st, id := lm.state, lm.leaseID
	if st == leaseHeld {
		lm.state = leaseNone
		lm.leaseID = ""
		lm.askedAt = time.Now()
		lm.resetLost() // signal the consume loop to stop
	}
	lm.mu.Unlock()
	if st != leaseHeld || id == "" {
		return
	}
	if err := lm.broker.Release(ctx, id); err != nil {
		lm.log.Printf("capworker lease: release %s: %v", id, err)
	}
}

// release gives back whatever the worker holds or is queued for (the
// shutdown path: the ledger is in-memory with no TTL, so an orphaned
// lease would block the restarted worker's fresh request).
func (lm *leaseManager) release(ctx context.Context) {
	lm.mu.Lock()
	st, id := lm.state, lm.leaseID
	if st == leaseHeld || st == leaseQueued {
		lm.state = leaseNone
		lm.leaseID = ""
	}
	lm.mu.Unlock()
	if id == "" {
		return
	}
	if err := lm.broker.Release(ctx, id); err != nil {
		lm.log.Printf("capworker lease: release %s: %v", id, err)
	}
}

// advanceLocked moves the state machine one step. Called with lm.mu
// held; it makes at most one broker call, rate-limited to one per poll
// interval (askedAt doubles as the rate limiter and the "when may I
// ask again" clock).
func (lm *leaseManager) advanceLocked(ctx context.Context) {
	now := time.Now()
	switch lm.state {
	case leaseNone:
		if !now.After(lm.askedAt.Add(lm.poll)) {
			return
		}
		lm.ask(ctx)
	case leaseQueued:
		if !now.After(lm.askedAt.Add(lm.poll)) {
			return
		}
		lm.askedAt = now
		presence, err := lm.broker.State(ctx, lm.leaseID)
		switch {
		case err != nil:
			lm.transitionFailOpen("state check failed", err)
		case presence == LeaseActive:
			lm.log.Printf("capworker lease: %s@%s granted after the queue (lease %s)", lm.spec.Capability, lm.spec.Device, lm.leaseID)
			lm.state = leaseHeld
			lm.resetLost()
		case presence == LeaseGone:
			// Withdrawn, denied by a re-evaluation, or resourced
			// restarted (its in-memory ledger forgot us): ask again.
			lm.state = leaseNone
			lm.leaseID = ""
			lm.askedAt = time.Time{}
		}
	case leaseHeld:
		if !now.After(lm.askedAt.Add(lm.poll)) {
			return
		}
		lm.askedAt = now
		presence, err := lm.broker.State(ctx, lm.leaseID)
		switch {
		case err != nil:
			// A network blip mid-hold does not mean the lease is gone
			// (resourced may well still hold it): keep consuming and
			// re-check on the next tick. Losing the lease over a blip
			// would thrash the consumer.
			lm.log.Printf("capworker lease: state check failed, keeping the lease: %v", err)
		case presence != LeaseActive:
			lm.log.Printf("capworker lease: %s@%s lost (state %s) — cancelling the consumer after the current delivery", lm.spec.Capability, lm.spec.Device, presence)
			lm.state = leaseNone
			lm.leaseID = ""
			lm.resetLost() // close the channel the consume loop watches
		}
	case leaseFailOpen:
		if !now.After(lm.askedAt.Add(lm.poll)) {
			return
		}
		lm.ask(ctx)
	}
}

// ask makes one lease request and folds the answer into the state
// machine. Called with lm.mu held.
func (lm *leaseManager) ask(ctx context.Context) {
	wasHolding := lm.state == leaseHeld || lm.state == leaseFailOpen
	d, err := lm.broker.Request(ctx, lm.spec)
	lm.askedAt = time.Now()
	switch {
	case err != nil:
		if lm.state != leaseFailOpen {
			lm.log.Printf("capworker lease: %s@%s: resourced unreachable (%v) — failing open, assuming granted", lm.spec.Capability, lm.spec.Device, err)
		}
		lm.state = leaseFailOpen
	case d.Status == LeaseGranted:
		lm.log.Printf("capworker lease: %s@%s granted (lease %s, %d MB VRAM / %d MB RAM)", lm.spec.Capability, lm.spec.Device, d.LeaseID, lm.spec.VRAMMB, lm.spec.RAMMB)
		lm.state = leaseHeld
		lm.leaseID = d.LeaseID
		if !wasHolding {
			lm.resetLost() // a fresh channel for the next consume session
		}
	case d.Status == LeaseQueued:
		lm.log.Printf("capworker lease: %s@%s queued (position %d) — waiting for a grant", lm.spec.Capability, lm.spec.Device, d.Position)
		lm.state = leaseQueued
		lm.leaseID = d.LeaseID
	default:
		lm.reason = d.Reason
		lm.log.Printf("capworker lease: %s@%s denied: %s", lm.spec.Capability, lm.spec.Device, d.Reason)
		lm.state = leaseNone
		lm.leaseID = ""
		if wasHolding {
			lm.resetLost() // the re-sync came back denied: stop consuming
		}
	}
}

// transitionFailOpen moves the queued state to fail-open (resourced
// unreachable mid-tracking). Called with lm.mu held. A held lease is
// left alone — a blip mid-hold does not lose the lease (the caller
// logged it).
func (lm *leaseManager) transitionFailOpen(what string, err error) {
	if lm.state != leaseQueued {
		return
	}
	lm.state = leaseFailOpen
	lm.log.Printf("capworker lease: %s@%s: resourced unreachable (%s: %v) — failing open, assuming granted", lm.spec.Capability, lm.spec.Device, what, err)
}

// resetLost closes the loss channel the consume loop watches (waking it
// to drain-and-cancel) and starts a fresh one for the next hold. Called
// with lm.mu held.
func (lm *leaseManager) resetLost() {
	close(lm.lostCh)
	lm.lostCh = make(chan struct{})
}
