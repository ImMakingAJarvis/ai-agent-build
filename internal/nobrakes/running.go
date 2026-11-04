package nobrakes

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"
)

// RunningKey marks the single auto job that is executing right now
// (NoBrakes 6/6, safety guardrails). Value: the job id. No TTL: the
// marker's lifetime is tied to the job's status in the job store, and a
// stuck marker fails safe (auto rounds idle-poll) rather than dangerous
// (two auto jobs at once). A stale marker is also taken over
// automatically: AcquireRunning treats a holder whose job is no longer
// running as dead (crash, reboot, a round that never released).
const RunningKey = "nobrakes:running"

// AcquireRunning tries to take the auto-run marker for jobID. It returns
// (holder, acquired, err):
//
//   - (jobID, true, nil)  — the marker is now held by jobID. This also
//     covers re-entry (the same job re-acquires, e.g. a round resumed
//     after a reboot) and the takeover of a stale marker.
//   - (other, false, nil) — another live auto job holds the marker; the
//     caller must not run an auto job until it is free.
//
// alive (nil for none) decides whether a foreign holder is still running;
// when it reports the holder dead, the stale marker is overwritten. The
// takeover is checked back against the key after the write, so two
// contenders racing on the same stale marker cannot both win.
func (s *Store) AcquireRunning(ctx context.Context, jobID string, alive func(string) bool) (string, bool, error) {
	ok, err := s.client.SetNX(ctx, RunningKey, jobID, 0).Result()
	if err != nil {
		return "", false, err
	}
	if ok {
		return jobID, true, nil
	}
	holder, err := s.client.Get(ctx, RunningKey).Result()
	if errors.Is(err, redis.Nil) {
		// The holder released between our SetNX and Get: try once more.
		ok, err := s.client.SetNX(ctx, RunningKey, jobID, 0).Result()
		if err != nil {
			return "", false, err
		}
		if ok {
			return jobID, true, nil
		}
		holder, err = s.client.Get(ctx, RunningKey).Result()
		if err != nil {
			return "", false, err
		}
	} else if err != nil {
		return "", false, err
	}
	if holder == jobID {
		return jobID, true, nil // re-entry
	}
	if alive != nil && !alive(holder) {
		// Stale marker: the holder is no longer running. Overwrite it and
		// verify the write still stands — a contender taking over the same
		// dead holder must not let both rounds run.
		if err := s.client.Set(ctx, RunningKey, jobID, 0).Err(); err != nil {
			return "", false, err
		}
		cur, err := s.client.Get(ctx, RunningKey).Result()
		if err != nil {
			return "", false, err
		}
		if cur != jobID {
			return cur, false, nil
		}
		return jobID, true, nil
	}
	return holder, false, nil
}

// ReleaseRunning clears the auto-run marker — but only when jobID holds
// it. Releasing a marker another job holds is a no-op, so a slow round
// can never clear a newer round's marker. A missing key (already
// released, or wiped) is not an error.
func (s *Store) ReleaseRunning(ctx context.Context, jobID string) error {
	cur, err := s.client.Get(ctx, RunningKey).Result()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur != jobID {
		return nil
	}
	return s.client.Del(ctx, RunningKey).Err()
}

// RunningHolder returns the id of the auto job that holds the marker, or
// "" when none does.
func (s *Store) RunningHolder(ctx context.Context) (string, error) {
	v, err := s.client.Get(ctx, RunningKey).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

// ClearRunning removes the marker unconditionally — the operator's manual
// override for a marker left behind by a crash (a stale marker is also
// taken over automatically when the holder's job is no longer running).
func (s *Store) ClearRunning(ctx context.Context) error {
	return s.client.Del(ctx, RunningKey).Err()
}
