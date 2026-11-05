// Package nobrakes is the Redis-backed flag store for the NoBrakes feature:
// autonomous job execution gated by operator-controlled flags — the nightly
// auto_mode batch, full auto mode (the loop runs any ready job at any
// time), and keep going (a failed auto job notifies but does not pause).
// Every flag is OFF by default and fully manual-overridable; see
// docs/nobrakes/redis.md for the key structure and where Redis runs.
package nobrakes

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis keys. The nobrakes: prefix namespaces all NoBrakes state.
const (
	// AutoModeKey holds the operator's auto_mode switch: "on" or "off".
	// A missing key reads as "off" — the safe default.
	AutoModeKey = "nobrakes:auto_mode"

	// NightlyFlagKey marks the nightly window. Value semantics:
	//
	//   ""          nightly enqueue has never run
	//   "YYYY-MM-DD" (UTC date) — the date of the last nightly window
	//   whose jobs were already enqueued
	//
	// The nightly gate (AutoEnqueue, run by `jarvis-jobs nightly` at the
	// top of every agent-run loop iteration) enqueues only when auto_mode
	// is "on" AND the flag is not today's UTC date; after enqueuing it
	// sets the flag to today's date. That makes the nightly batch
	// idempotent: loop restarts or repeated checks on the same day cannot
	// double-enqueue. The key has no TTL — it is compared across days.
	NightlyFlagKey = "nobrakes:nightly_flag"

	// FullAutoKey holds the full-auto switch: "on" or "off". When "on",
	// the agent-run loop runs the next ready job immediately — at any
	// time of day, not just the nightly window — for as long as the queue
	// has work; with an empty queue it idle-polls instead of asking. It
	// is independent of auto_mode (the nightly batch) and can be
	// combined with it. Read by the agent-run loop on every iteration;
	// written by the operator (CLI `jarvis-jobs fullauto on|off`,
	// Discord !fullauto). A missing key reads as "off" — the safe default.
	FullAutoKey = "nobrakes:full_auto"

	// KeepGoingKey holds the keep-going switch: "on" or "off". When "on",
	// a failed auto-executed job does NOT pause autonomous execution:
	// the owner is still notified, auto_mode/full_auto stay as they are,
	// and the loop moves on to the next ready job (the failed job stays
	// "failed" in the store and is not re-picked). When "off" (default),
	// an auto failure trips autopause as usual. It is a modifier on the
	// failure path, not an execution switch — it starts nothing by
	// itself. Read by the agent-run loop on the auto-failure path;
	// written by the operator (CLI `jarvis-jobs keepgoing on|off`,
	// Discord !keepgoing, the checkpoint's "keep going" button). A
	// missing key reads as "off" — the safe default.
	KeepGoingKey = "nobrakes:keep_going"
)

// Flag values, shared by every NoBrakes switch.
const (
	AutoModeOn  = "on"
	AutoModeOff = "off"
)

// Store reads and writes the NoBrakes flag state in Redis.
type Store struct {
	client *redis.Client
}

// New dials the Redis at addr (a redis:// URL, as in $JARVIS_REDIS) and
// verifies the connection. It returns an error when Redis is unreachable.
func New(addr string) (*Store, error) {
	opt, err := redis.ParseURL(addr)
	if err != nil {
		return nil, err
	}
	// The caller (the nightly gate) runs this once per agent-run iteration
	// and fails safe on error. No command retries (-1: 0 means the library
	// default of 3): a dead Redis must fail the probe fast — one dial
	// attempt, one pool log line — since the next loop iteration will try
	// again anyway.
	opt.MaxRetries = -1
	client := redis.NewClient(opt)
	if err := client.Ping(context.Background()).Err(); err != nil {
		client.Close()
		return nil, err
	}
	return &Store{client: client}, nil
}

// NewFromClient wraps an existing client (used by tests and callers that
// already hold one).
func NewFromClient(client *redis.Client) *Store {
	return &Store{client: client}
}

// Close releases the connection pool.
func (s *Store) Close() error { return s.client.Close() }

// getFlag reads one of the on/off flag keys. A missing key reads as "off"
// so a fresh or wiped Redis is safe by default; a corrupt value is
// treated the same rather than trusted.
func (s *Store) getFlag(ctx context.Context, key string) (string, error) {
	v, err := s.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return AutoModeOff, nil
	}
	if err != nil {
		return AutoModeOff, err
	}
	if v != AutoModeOn && v != AutoModeOff {
		// Corrupt value: treat as off rather than trusting it.
		return AutoModeOff, nil
	}
	return v, nil
}

// GetAutoMode returns the current auto_mode value. A missing key reads as
// "off" so a fresh or wiped Redis is safe by default.
func (s *Store) GetAutoMode(ctx context.Context) (string, error) {
	return s.getFlag(ctx, AutoModeKey)
}

// GetFullAuto returns the current full-auto value. A missing key reads as
// "off" so a fresh or wiped Redis is safe by default.
func (s *Store) GetFullAuto(ctx context.Context) (string, error) {
	return s.getFlag(ctx, FullAutoKey)
}

// AutoModeOn reports whether autonomous nightly execution is enabled.
func (s *Store) AutoModeOn(ctx context.Context) (bool, error) {
	v, err := s.GetAutoMode(ctx)
	return v == AutoModeOn, err
}

// SetAutoMode sets the auto_mode switch to the given value. This is the
// manual-override path (e.g. the Discord !automode command in nobrakes-3).
func (s *Store) SetAutoMode(ctx context.Context, on bool) error {
	if on {
		return s.client.Set(ctx, AutoModeKey, AutoModeOn, 0).Err()
	}
	return s.client.Set(ctx, AutoModeKey, AutoModeOff, 0).Err()
}

// FullAutoOn reports whether full auto mode is enabled: the agent-run loop
// runs every ready job immediately, at any time of day, as long as the
// queue has work.
func (s *Store) FullAutoOn(ctx context.Context) (bool, error) {
	v, err := s.GetFullAuto(ctx)
	return v == AutoModeOn, err
}

// SetFullAuto sets the full-auto switch to the given value. This is the
// manual-override path (the CLI `jarvis-jobs fullauto on|off` and the
// Discord !fullauto command).
func (s *Store) SetFullAuto(ctx context.Context, on bool) error {
	if on {
		return s.client.Set(ctx, FullAutoKey, AutoModeOn, 0).Err()
	}
	return s.client.Set(ctx, FullAutoKey, AutoModeOff, 0).Err()
}

// GetKeepGoing returns the current keep-going value. A missing key reads
// as "off" — the safe default (an auto failure pauses the feature).
func (s *Store) GetKeepGoing(ctx context.Context) (string, error) {
	return s.getFlag(ctx, KeepGoingKey)
}

// KeepGoingOn reports whether keep-going is enabled: a failed auto job
// notifies the owner but does not pause autonomous execution.
func (s *Store) KeepGoingOn(ctx context.Context) (bool, error) {
	v, err := s.GetKeepGoing(ctx)
	return v == AutoModeOn, err
}

// SetKeepGoing sets the keep-going switch to the given value. Like the
// other switches it is the operator's manual override: a direct Redis
// write that works at any time, including mid-round.
func (s *Store) SetKeepGoing(ctx context.Context, on bool) error {
	if on {
		return s.client.Set(ctx, KeepGoingKey, AutoModeOn, 0).Err()
	}
	return s.client.Set(ctx, KeepGoingKey, AutoModeOff, 0).Err()
}

// GetNightlyFlag returns the last processed nightly window (UTC date
// "YYYY-MM-DD"), or "" when the nightly enqueue has never run.
func (s *Store) GetNightlyFlag(ctx context.Context) (string, error) {
	v, err := s.client.Get(ctx, NightlyFlagKey).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

// SetNightlyFlag records that the nightly window for date (UTC "YYYY-MM-DD")
// has been processed. Callers pass time.Now().UTC().Format("2006-01-02").
func (s *Store) SetNightlyFlag(ctx context.Context, date string) error {
	return s.client.Set(ctx, NightlyFlagKey, date, 0).Err()
}

// NightlyDue reports whether the nightly window for now is still unprocessed:
// the flag is not today's UTC date. Callers still gate on AutoModeOn — this
// only answers the idempotency question.
func (s *Store) NightlyDue(ctx context.Context, now time.Time) (bool, error) {
	flag, err := s.GetNightlyFlag(ctx)
	if err != nil {
		return false, err
	}
	return flag != now.UTC().Format("2006-01-02"), nil
}

// Pause turns every NoBrakes switch off — auto_mode and full_auto. It is
// the error path for auto-execution (nobrakes-6): an auto job that failed
// stops ALL autonomous execution until the operator re-enables a switch
// manually (!automode on / !fullauto on). The feature never re-enables
// itself. A missing key already reads as off, so a caller that cannot
// reach Redis still ends up safe; this just makes the state explicit.
//
// keep_going is deliberately NOT touched: it is a modifier on the failure
// path, not an execution switch — with both switches off no auto job can
// run, so there is no failure left for keep_going to change.
func (s *Store) Pause(ctx context.Context) error {
	if err := s.SetAutoMode(ctx, false); err != nil {
		return err
	}
	return s.SetFullAuto(ctx, false)
}

// EnsureDefaults creates all flag keys with their safe defaults if (and
// only if) they are missing: auto_mode "off", nightly_flag "",
// full_auto "off", keep_going "off". Idempotent, so callers may run it
// at startup without clobbering operator state.
func (s *Store) EnsureDefaults(ctx context.Context) error {
	if err := s.client.SetNX(ctx, AutoModeKey, AutoModeOff, 0).Err(); err != nil {
		return err
	}
	if err := s.client.SetNX(ctx, NightlyFlagKey, "", 0).Err(); err != nil {
		return err
	}
	if err := s.client.SetNX(ctx, FullAutoKey, AutoModeOff, 0).Err(); err != nil {
		return err
	}
	return s.client.SetNX(ctx, KeepGoingKey, AutoModeOff, 0).Err()
}

// Owner-facing confirmations for the switches. Shared by every control
// surface (the Discord !automode / !fullauto commands in cmd/jarvis-chat,
// the brain's set_automode / set_fullauto actions, and the CLI) so the
// owner hears the same words no matter how they flipped the switch.

// AutoModeReply builds the confirmation for the auto_mode state. Turning
// the switch on warns that nightly jobs will now run autonomously.
func AutoModeReply(on bool) string {
	if on {
		return "Auto mode is ON — nightly jobs (tagged \"nightly\") will auto-enqueue " +
			"at the top of the next agent-run loop iteration and run once per UTC day. " +
			"Say !automode off to stop this."
	}
	return "Auto mode is OFF — nightly jobs will not auto-enqueue."
}

// FullAutoReply builds the confirmation for the full-auto state. Turning
// the switch on warns that queued jobs will now run autonomously at any
// time, and how to stop it.
func FullAutoReply(on bool) string {
	if on {
		return "Full auto is ON — the agent-run loop will run each ready job " +
			"immediately (driver from the job's spec), at any time of day, for " +
			"as long as the queue has work; it idle-polls when the queue is " +
			"empty. Say !fullauto off to stop; !pause and !restart still work " +
			"mid-run."
	}
	return "Full auto is OFF — the loop asks on Discord before running each job."
}

// KeepGoingReply builds the confirmation for the keep-going state. Turning
// the switch on warns that a failed auto job will not stop the loop, and
// that it only changes what happens on failure — it starts nothing by
// itself.
func KeepGoingReply(on bool) string {
	if on {
		return "Keep going is ON — when an auto-executed job fails, you will be " +
			"notified but the loop keeps working through the next ready job " +
			"instead of pausing. It only matters while auto_mode or full auto " +
			"is running. Say !keepgoing off to pause on failure again."
	}
	return "Keep going is OFF — a failed auto job pauses autonomous execution " +
		"and notifies you, as usual."
}
