package kshadow

// Phase 2 of docs/kafka-migration-plan.md §5: the shadow dry-run.
//
// For every queue family a dry-run consumer reads the mirrored Kafka
// topics and records what the phase-3 Kafka consumer WOULD do (deliver
// job X to the worker, wake agent job Y, run capability Z, expire an
// overdue envelope), then each would-do is matched against what the AMQP
// side actually did — the job.* / agent.* lifecycle events on the events
// topic (mirrored from the events exchange). The gate: 7 days, every
// would-do matches a real event. Families without per-message events
// (events, sched.*, review.*) are checked for well-formedness; their
// live-consumer drain is already the parity table's Consumed column.
//
// The dry-run proves the two semantics Kafka replaces directly:
//
//   - plan §3.2 priority routing: the worker family's lanes poll
//     high-priority topic first (voice.p10 → voice, ...), mirroring the
//     AMQP worker's per-queue prefetch-1 consumers, and every
//     job.*.started is checked against that order (inversions).
//   - plan §3.3 TTL janitor: every envelope with a past deadline that
//     never matched is counted as a simulated expiry (would-expire) and
//     compared against the real arrivals on the dead-letter queue.
//
// It never touches a live consumer: it is a reader of the mirrored
// topics and the management API only.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kq"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// Match modes — how a family's would-dos are checked against the real side.
const (
	matchEvent = "event" // expect a job.<cap>.* event with job_id == envelope id
	matchAgent = "agent" // expect an agent.<job-id>.* event with job_id
	matchDepth = "depth" // no per-message event: well-formedness only
)

// concurrentWindow bounds the inversion check: only messages published
// within this window of each other were "available at the same time" on
// the AMQP side (a job published hours earlier was legitimately first).
const concurrentWindow = 5 * time.Minute

// deadQueue is the AMQP dead-letter queue (jarvis.dlx fanout → dead):
// the real side of the janitor comparison.
const deadQueue = "dead"

// Family is one cutover family (plan §5 order). Static is a fixed topic
// set; Prefix/Suffix are resolved live from the broker (on-demand
// topics). Bases groups topics into polling lanes, one per consumer:
// the worker family mirrors the AMQP worker's one-goroutine-per-queue,
// prefetch-1 layout, each lane polling in list order (high priority
// first, plan §3.2).
type Family struct {
	Name   string
	Mode   string
	Static []string
	Prefix string
	Suffix string
	Bases  [][]string
	Stale  time.Duration // before an unmatched would-do is counted stale
}

// Families is the eight queue families in plan §5 cutover order.
func Families() []Family {
	return []Family{
		{Name: "events", Mode: matchDepth, Static: []string{EventsQueue}},
		{Name: "sched", Mode: matchDepth,
			Static: []string{"sched.hourly", "sched.daily", "sched.weekly", "sched.monthly"}},
		{Name: "review", Mode: matchDepth,
			Static: []string{"review.sonnet", "review.opus", "review.fable", "review.local"}},
		{Name: "mailboxes", Mode: matchAgent, Prefix: "agent.", Suffix: ".inbox", Stale: 30 * time.Minute},
		{Name: "cap", Mode: matchEvent, Prefix: "cap.", Stale: 30 * time.Minute},
		{Name: "flow", Mode: matchAgent, Prefix: "flow.", Stale: 30 * time.Minute},
		{Name: "jobs", Mode: matchEvent, Static: []string{"jobs.gpu", "jobs.cpu", "jobs.igpu"}, Stale: 4 * time.Hour},
		{
			Name:  "worker",
			Mode:  matchEvent,
			Stale: 4 * time.Hour,
			Bases: [][]string{
				{"voice.p10", "voice"},
				{"image.p10", "image"},
				{"coding.p5", "coding"},
				{"background.p10", "background"},
				{"fix"},
			},
		},
	}
}

// familyStale is the stale window for a family (match families only
// have one set; the default is 30 m).
func familyStale(name string) time.Duration {
	for _, f := range Families() {
		if f.Name == name && f.Stale > 0 {
			return f.Stale
		}
	}
	return 30 * time.Minute
}

// PendingEntry is one would-do the dry-run is still waiting to match
// against a real event (or to count as a simulated TTL expiry). It is
// persisted, so a kshadow restart does not forget the gate's
// obligations.
type PendingEntry struct {
	Family   string     `json:"family"`
	Topic    string     `json:"topic"`
	Ts       time.Time  `json:"ts"` // the mirrored record's time (the "publish" time)
	Deadline *time.Time `json:"deadline,omitempty"`
	Prio     int        `json:"prio,omitempty"`
	Base     string     `json:"base,omitempty"` // worker family: the base queue
	Expired  bool       `json:"expired,omitempty"`
}

// FamilyCounters is one family's dry-run accounting. Persisted, so the
// 7-day gate spans restarts.
type FamilyCounters struct {
	Seen        int64     `json:"seen"`
	Matched     int64     `json:"matched"`
	Late        int64     `json:"late"` // matched after the stale window
	Stale       int64     `json:"stale"`
	Inversions  int64     `json:"inversions"` // priority/FIFO violations (worker family)
	WouldExpire int64     `json:"would_expire"`
	Malformed   int64     `json:"malformed"`
	LastAction  string    `json:"last_action,omitempty"`
	LastAt      time.Time `json:"last_at,omitempty"`
}

// DryRunState is the dry-run's persisted state (a sibling of the
// mirror's state file). Offlets let a restart resume where it stopped;
// the counters are the gate itself.
type DryRunState struct {
	StartedAt time.Time                `json:"started_at"`
	Offsets   map[string]int64         `json:"offsets"` // topic -> next offset to read
	Families  map[string]*FamilyCounters `json:"families"`
	Pending   map[string]*PendingEntry `json:"pending"`
	Stale     map[string]*PendingEntry `json:"stale"`
	DeadBase  int64                    `json:"dead_base"` // dead queue (ready+unacked+delivered) at first sweep
	DeadNew   int64                    `json:"dead_new"`  // arrivals on dead since then
}

// NewDryRunState returns an empty state stamped with the first-seen time.
func NewDryRunState() *DryRunState {
	return &DryRunState{
		StartedAt: time.Now().UTC(),
		Offsets:   map[string]int64{},
		Families:  map[string]*FamilyCounters{},
		Pending:   map[string]*PendingEntry{},
		Stale:     map[string]*PendingEntry{},
	}
}

// Counter returns (creating if needed) a family's counters.
func (s *DryRunState) Counter(family string) *FamilyCounters {
	c, ok := s.Families[family]
	if !ok {
		c = &FamilyCounters{}
		s.Families[family] = c
	}
	return c
}

// Save writes the state atomically (temp file + rename).
func (s *DryRunState) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("dryrun state: mkdir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("dryrun state: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("dryrun state: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("dryrun state: rename: %w", err)
	}
	return nil
}

// LoadDryRunState reads persisted dry-run state; a missing or unreadable
// file yields a fresh state.
func LoadDryRunState(path string) (*DryRunState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewDryRunState(), nil
		}
		return nil, fmt.Errorf("dryrun state: read %s: %w", path, err)
	}
	var s DryRunState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("dryrun state: unmarshal %s: %w", path, err)
	}
	if s.Offsets == nil {
		s.Offsets = map[string]int64{}
	}
	if s.Families == nil {
		s.Families = map[string]*FamilyCounters{}
	}
	if s.Pending == nil {
		s.Pending = map[string]*PendingEntry{}
	}
	if s.Stale == nil {
		s.Stale = map[string]*PendingEntry{}
	}
	return &s, nil
}

// DryRun runs the dry-run consumers for all families.
type DryRun struct {
	brokers string
	kafka   *kq.Client
	admin   *Admin
	state   *DryRunState
	path    string
	log     *log.Logger

	mu       sync.Mutex // guards pending, stale, matchLog, seen, state
	pending  map[string]*PendingEntry
	stale    map[string]*PendingEntry
	matchLog []matchRec // worker family: recently started jobs (inversion check)
	seen     map[string]bool
	lastSave time.Time
	lastErr  time.Time
}

// matchRec is one started job kept for the worker family's inversion
// check (a pair is only comparable while both are recent).
type matchRec struct {
	JobID string
	Ts    time.Time // publish time
	Prio  int
	Base  string
	Start time.Time // the job.*.started event's time
}

// NewDryRun builds a DryRun from the client, the management API (nil is
// tolerated: the janitor comparison just stays empty) and the persisted
// state, rehydrating the pending/stale sets.
func NewDryRun(brokers string, kafka *kq.Client, admin *Admin, state *DryRunState, path string, logf *log.Logger) *DryRun {
	return &DryRun{
		brokers: brokers,
		kafka:   kafka,
		admin:   admin,
		state:   state,
		path:    path,
		log:     logf,
		pending: state.Pending,
		stale:   state.Stale,
		seen:    map[string]bool{},
	}
}

// Run reads every family until ctx is done. Readers start at the log end
// (records already in the log are pre-gate test traffic from phases
// 0–1; the 7-day gate is measured from now) and persist per-topic
// offsets so a restart resumes where it stopped.
func (d *DryRun) Run(ctx context.Context) error {
	d.log.Printf("dry-run starting (families=%d, pending=%d, stale=%d)",
		len(Families()), len(d.pending), len(d.stale))
	var wg sync.WaitGroup
	for _, f := range Families() {
		f := f
		if len(f.Bases) > 0 {
			for _, base := range f.Bases {
				wg.Add(1)
				go func(lane []string) { defer wg.Done(); d.laneLoop(ctx, f, lane) }(base)
			}
		} else {
			wg.Add(1)
			go func() { defer wg.Done(); d.laneLoop(ctx, f, f.Static) }()
		}
	}
	wg.Add(1)
	go func() { defer wg.Done(); d.eventLoop(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); d.sweepLoop(ctx) }()
	wg.Wait()
	if err := d.state.Save(d.path); err != nil {
		d.log.Printf("save dry-run state: %v", err)
	}
	return nil
}

// topicReader is one topic's fetcher: a kafka-go reader feeding a
// capacity-1 channel (prefetch 1). hub is the lane's wake signal.
type topicReader struct {
	topic string
	r     *kafka.Reader
	ch    chan kafka.Message
	hub   chan *topicReader
	done  chan struct{}
}

func (tr *topicReader) loop(ctx context.Context, d *DryRun, onOffsetErr func()) {
	for {
		rec, err := tr.r.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if strings.Contains(err.Error(), "offset") {
				d.log.Printf("dry-run: topic %s: %v — resetting to log start", tr.topic, err)
				onOffsetErr()
				return
			}
			d.logErr(tr.topic, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		select {
		case tr.ch <- rec:
		case <-tr.done:
			return
		}
	}
}

// laneLoop polls one consumer lane: its topics in list order (high
// priority first for the worker family — the phase-3 consumer's layout),
// prefetch 1 per topic.
func (d *DryRun) laneLoop(ctx context.Context, f Family, lane []string) {
	hub := make(chan *topicReader, len(lane)+8)
	readers := map[string]*topicReader{}
	var mu sync.Mutex
	defer func() {
		mu.Lock()
		for _, tr := range readers {
			select {
			case <-tr.done:
			default:
				close(tr.done)
			}
			_ = tr.r.Close()
		}
		mu.Unlock()
	}()
	for {
		var topics []string
		if f.Prefix != "" {
			topics = d.dynamicTopics(ctx, f)
		} else {
			for _, t := range lane {
				if d.topicExists(ctx, t) {
					topics = append(topics, t)
				}
			}
		}
		mu.Lock()
		for t, tr := range readers {
			if !containsString(topics, t) {
				close(tr.done)
				delete(readers, t)
			}
		}
		for _, t := range topics {
			if _, ok := readers[t]; ok {
				continue
			}
			tr := &topicReader{
				topic: t,
				r:     d.newReader(t, d.startOffset(t)),
				ch:    make(chan kafka.Message, 1),
				hub:   hub,
				done:  make(chan struct{}),
			}
			readers[t] = tr
			topic := t
			go tr.loop(ctx, d, func() {
				// Offset past the log end (a purge or the plan §7
				// rollback): drop the reader so the lane recreates it at
				// the log end and forgets the offset — purged records
				// could never match (their events are purged too), so
				// skipping them keeps the gate clean.
				mu.Lock()
				if cur, ok := readers[topic]; ok && cur == tr {
					close(cur.done)
					delete(readers, topic)
				}
				mu.Unlock()
				d.mu.Lock()
				d.state.Offsets[topic] = 0
				d.mu.Unlock()
			})
		}
		mu.Unlock()

	stall:
		for {
			progress := false
			for _, t := range topics {
				mu.Lock()
				tr, ok := readers[t]
				mu.Unlock()
				if !ok {
					continue
				}
				select {
				case rec := <-tr.ch:
					d.process(f, rec)
					progress = true
				default:
				}
			}
			if progress {
				continue
			}
			stalled := false
			select {
			case <-hub:
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
				stalled = true
			}
			if stalled {
				break stall // re-resolve the topic set
			}
		}
	}
}

// process records one mirrored record's would-do for family f and leaves
// it in pending for the event side to match. Idempotent by message id
// (plan §7.2): a re-read record counts once.
func (d *DryRun) process(f Family, rec kafka.Message) {
	now := time.Now().UTC()
	var amqpID string
	for _, h := range rec.Headers {
		if h.Key == "amqp-message-id" {
			amqpID = string(h.Value)
		}
	}
	id := MessageID(amqpID, rec.Value)
	d.mu.Lock()
	if d.seen[id] {
		d.mu.Unlock()
		return
	}
	if len(d.seen) > 50000 { // replay safety valve
		d.seen = map[string]bool{}
	}
	d.seen[id] = true
	c := d.state.Counter(f.Name)
	c.Seen++
	d.mu.Unlock()

	var m queue.Message
	if err := json.Unmarshal(rec.Value, &m); err != nil {
		d.note(f.Name, "malformed "+rec.Topic, now, func(c *FamilyCounters) { c.Malformed++ })
		return
	}
	if f.Mode == matchDepth {
		d.note(f.Name, "ok "+rec.Topic, now, nil)
		return
	}
	jobID := d.matchKey(f, rec.Topic, m, id)
	if jobID == "" {
		d.note(f.Name, "no-id "+rec.Topic, now, func(c *FamilyCounters) { c.Malformed++ })
		return
	}
	ts := rec.Time
	if ts.IsZero() {
		ts = now
	}
	var prio int
	var base string
	if f.Name == "worker" {
		prio, base = prioBase(rec.Topic)
	}
	d.mu.Lock()
	if _, dup := d.pending[jobID]; !dup {
		d.pending[jobID] = &PendingEntry{
			Family: f.Name, Topic: rec.Topic, Ts: ts,
			Deadline: m.Deadline, Prio: prio, Base: base,
		}
	}
	d.mu.Unlock()
	d.note(f.Name, "would-do "+rec.Topic+" "+jobID, now, nil)
}

// matchKey is the id the real side's event carries for this would-do:
// the envelope id for task-style families (job.* events use it), the
// topic's job id for flow.*, the wake job for mailboxes.
func (d *DryRun) matchKey(f Family, topic string, m queue.Message, id string) string {
	switch f.Name {
	case "flow":
		return flowJobID(topic)
	case "mailboxes":
		if m.WakeJob != "" {
			return m.WakeJob
		}
		return firstNonEmpty(m.ID, id)
	default:
		return firstNonEmpty(m.ID, id)
	}
}

// note stamps a family's last action (and bumps a counter when given).
func (d *DryRun) note(family, action string, at time.Time, bump func(*FamilyCounters)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.state.Counter(family)
	if bump != nil {
		bump(c)
	}
	c.LastAction = action
	c.LastAt = at
}

// eventLoop reads the mirrored events topic and matches job.*/agent.*
// events against pending would-dos — the gate: every would-do must find
// its real event.
func (d *DryRun) eventLoop(ctx context.Context) {
	r := d.newReader(EventsQueue, d.startOffset(EventsQueue))
	defer r.Close()
	for {
		rec, err := r.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if strings.Contains(err.Error(), "offset") {
				d.log.Printf("dry-run: events: %v — resetting to log start", err)
				d.mu.Lock()
				d.state.Offsets[EventsQueue] = 0
				d.mu.Unlock()
				r.Close()
				r = d.newReader(EventsQueue, kafka.FirstOffset)
				continue
			}
			d.logErr(EventsQueue, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		var e queue.Event
		if json.Unmarshal(rec.Value, &e) == nil && e.JobID != "" &&
			(e.Kind == queue.JobKind || e.Kind == queue.AgentKind) {
			d.match(e)
		}
		d.mu.Lock()
		d.state.Offsets[EventsQueue] = rec.Offset + 1
		d.mu.Unlock()
	}
}

// match resolves one real event against the pending would-dos.
func (d *DryRun) match(e queue.Event) {
	now := time.Now().UTC()
	d.mu.Lock()
	defer d.mu.Unlock()
	if wd, ok := d.pending[e.JobID]; ok {
		delete(d.pending, e.JobID)
		c := d.state.Counter(wd.Family)
		c.Matched++
		c.LastAction = "matched " + e.RoutingKey()
		c.LastAt = now
		if wd.Family == "worker" {
			d.checkInversion(e.JobID, wd, e.At)
		}
		return
	}
	if wd, ok := d.stale[e.JobID]; ok {
		delete(d.stale, e.JobID)
		c := d.state.Counter(wd.Family)
		c.Late++
		c.LastAction = "late-matched " + e.RoutingKey()
		c.LastAt = now
	}
}

// checkInversion counts a priority/FIFO inversion if this just-matched
// would-do started after a message that should have gone first (same
// base queue, published within concurrentWindow). Called with d.mu held.
func (d *DryRun) checkInversion(jobID string, wd *PendingEntry, start time.Time) {
	c := d.state.Counter("worker")
	for _, b := range d.matchLog {
		if isPriorityInversion(wd, &b, start) {
			c.Inversions++
			c.LastAction = "inversion " + wd.Topic + " (started after " + b.JobID + ")"
			c.LastAt = start
		}
	}
	cutoff := start.Add(-10 * time.Minute)
	keep := d.matchLog[:0]
	for _, m := range d.matchLog {
		if m.Start.After(cutoff) {
			keep = append(keep, m)
		}
	}
	d.matchLog = append(keep, matchRec{JobID: jobID, Ts: wd.Ts, Prio: wd.Prio, Base: wd.Base, Start: start})
}

// isPriorityInversion reports whether a (started at start, after b
// started) violates the order the AMQP worker would produce: same base
// queue, both published within concurrentWindow, and a was published no
// later at equal-or-higher priority — so a should have started first.
func isPriorityInversion(a *PendingEntry, b *matchRec, start time.Time) bool {
	if a.Base == "" || a.Base != b.Base {
		return false
	}
	if !start.After(b.Start) {
		return false
	}
	if a.Ts.After(b.Ts.Add(concurrentWindow)) || b.Ts.After(a.Ts.Add(concurrentWindow)) {
		return false
	}
	if a.Ts.After(b.Ts) {
		return false
	}
	return a.Prio > b.Prio || (a.Prio == b.Prio && a.Ts.Before(b.Ts))
}

// sweepLoop ages out unmatched would-dos, counts simulated TTL
// expiries, refreshes the dead queue's arrival count and saves state.
func (d *DryRun) sweepLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.sweepOnce(ctx, time.Now().UTC())
		}
	}
}

// sweepOnce is one sweep pass (factored out for tests).
func (d *DryRun) sweepOnce(ctx context.Context, now time.Time) {
	d.mu.Lock()
	for id, wd := range d.pending {
		if now.Sub(wd.Ts) > familyStale(wd.Family) {
			d.stale[id] = wd
			delete(d.pending, id)
			d.state.Counter(wd.Family).Stale++
		} else if wd.Deadline != nil && !wd.Expired && now.After(*wd.Deadline) {
			wd.Expired = true
			d.state.Counter(wd.Family).WouldExpire++
		}
	}
	for _, wd := range d.stale {
		if wd.Deadline != nil && !wd.Expired && now.After(*wd.Deadline) {
			wd.Expired = true
			d.state.Counter(wd.Family).WouldExpire++
		}
	}
	d.mu.Unlock()

	// The real side of the janitor: arrivals on the dead-letter queue
	// (nothing consumes dead, so ready+unacked+delivered is the count).
	if d.admin != nil {
		if st, err := d.admin.QueueStats(ctx, deadQueue); err == nil {
			cur := st.Ready + st.Unacked + st.Delivered
			d.mu.Lock()
			if d.state.DeadBase == 0 {
				d.state.DeadBase = cur
				d.state.DeadNew = 0
			} else {
				d.state.DeadNew = cur - d.state.DeadBase
			}
			d.mu.Unlock()
		}
	}
	d.save()
}

// save persists state, throttled to one write per 5 s.
func (d *DryRun) save() {
	d.mu.Lock()
	if time.Since(d.lastSave) < 5*time.Second {
		d.mu.Unlock()
		return
	}
	d.lastSave = time.Now()
	d.mu.Unlock()
	if err := d.state.Save(d.path); err != nil {
		d.log.Printf("save dry-run state: %v", err)
	}
}

// dynamicTopics lists the family's on-demand topics (prefix/suffix
// match against the broker's topic list).
func (d *DryRun) dynamicTopics(ctx context.Context, f Family) []string {
	list, err := d.kafka.Topics(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, ti := range list {
		if ti.Error != nil {
			continue
		}
		if strings.HasPrefix(ti.Name, f.Prefix) && (f.Suffix == "" || strings.HasSuffix(ti.Name, f.Suffix)) {
			out = append(out, ti.Name)
		}
	}
	sort.Strings(out)
	return out
}

// topicExists reports whether the broker knows the topic. An unknown
// answer is "yes": the reader retries rather than the lane dropping it.
func (d *DryRun) topicExists(ctx context.Context, topic string) bool {
	list, err := d.kafka.Topics(ctx)
	if err != nil {
		return true
	}
	for _, ti := range list {
		if ti.Name == topic {
			return true
		}
	}
	return false
}

// startOffset is a topic's next-read offset: the persisted offset when
// there is one (a restart), else the log end (fresh — existing records
// are pre-gate test traffic).
func (d *DryRun) startOffset(topic string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if o, ok := d.state.Offsets[topic]; ok && o > 0 {
		return o
	}
	return kafka.LastOffset
}

// newReader builds a plain (group-less) reader: the dry-run is a single
// instance and its own persisted offsets are the commit mechanism.
//
// kafka-go v0.4.51 applies ReaderConfig.StartOffset only to consumer-group
// readers; a plain reader starts from FirstOffset unless positioned with
// SetOffset before the first read (reader.go: NewReader seeds r.offset =
// FirstOffset, FetchMessage starts there). SetOffset is resolved in the
// reader's initialize: LastOffset → the log end (new messages only),
// FirstOffset → the log start, n → offset n.
func (d *DryRun) newReader(topic string, off int64) *kafka.Reader {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  strings.Split(d.brokers, ","),
		Topic:    topic,
		MinBytes: 1,
		MaxBytes: 1 << 20,
	})
	if err := r.SetOffset(off); err != nil {
		d.log.Printf("dry-run: topic %s: set offset %d: %v", topic, off, err)
	}
	return r
}

func (d *DryRun) logErr(topic string, err error) {
	d.mu.Lock()
	throttled := time.Since(d.lastErr) > 30*time.Second
	d.lastErr = time.Now()
	d.mu.Unlock()
	if throttled {
		d.log.Printf("dry-run: topic %s: %v", topic, err)
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// flowJobID extracts the agentjob id from a flow topic name (flow.<job>
// or flow.<job>.default, the latter when the AMQP queue carried a
// profile suffix).
func flowJobID(topic string) string {
	t := strings.TrimPrefix(topic, "flow.")
	return strings.TrimSuffix(t, ".default")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// prioBase maps a worker topic to (priority, base queue): voice.p10 →
// (10, voice); fix → (0, fix).
func prioBase(topic string) (prio int, base string) {
	if i := strings.LastIndexByte(topic, '.'); i > 0 && topic[i+1] == 'p' {
		if p, err := strconv.Atoi(topic[i+2:]); err == nil {
			return p, topic[:i]
		}
	}
	return 0, topic
}
