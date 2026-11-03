package nobrakes

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	mr := miniredis.RunT(t)
	t.Cleanup(func() { mr.Close() })
	return NewFromClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
}

func TestGetAutoModeMissingKeyIsOff(t *testing.T) {
	s := newTestStore(t)
	v, err := s.GetAutoMode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != AutoModeOff {
		t.Fatalf("missing key = %q, want %q", v, AutoModeOff)
	}
	on, err := s.AutoModeOn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if on {
		t.Error("AutoModeOn with missing key = true, want false")
	}
}

func TestSetAutoModeRoundtrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SetAutoMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetAutoMode(ctx); v != AutoModeOn {
		t.Fatalf("after on: %q", v)
	}
	on, _ := s.AutoModeOn(ctx)
	if !on {
		t.Error("AutoModeOn = false after SetAutoMode(true)")
	}
	if err := s.SetAutoMode(ctx, false); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetAutoMode(ctx); v != AutoModeOff {
		t.Fatalf("after off: %q", v)
	}
}

func TestGetAutoModeCorruptValueIsOff(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.client.Set(ctx, AutoModeKey, "banana", 0).Err(); err != nil {
		t.Fatal(err)
	}
	v, err := s.GetAutoMode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v != AutoModeOff {
		t.Fatalf("corrupt value = %q, want %q", v, AutoModeOff)
	}
}

func TestNightlyFlag(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if v, err := s.GetNightlyFlag(ctx); err != nil || v != "" {
		t.Fatalf("initial flag = %q, %v; want empty", v, err)
	}
	today := time.Date(2026, 11, 3, 23, 30, 0, 0, time.UTC)
	if due, err := s.NightlyDue(ctx, today); err != nil || !due {
		t.Fatalf("NightlyDue fresh = %v, %v; want true", due, err)
	}
	if err := s.SetNightlyFlag(ctx, "2026-11-03"); err != nil {
		t.Fatal(err)
	}
	// Same day, any time of day (UTC): no longer due — idempotent.
	for _, tt := range []time.Time{
		time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 3, 23, 59, 59, 0, time.UTC),
	} {
		if due, err := s.NightlyDue(ctx, tt); err != nil || due {
			t.Fatalf("NightlyDue(%s) = %v, %v; want false", tt, due, err)
		}
	}
	// Next day is due again.
	if due, err := s.NightlyDue(ctx, time.Date(2026, 11, 4, 2, 0, 0, 0, time.UTC)); err != nil || !due {
		t.Fatalf("NightlyDue next day = %v, %v; want true", due, err)
	}
	// A local (non-UTC) now is normalized to UTC before comparing:
	// 02:30 in UTC+2 is 00:30 UTC the same day.
	if due, err := s.NightlyDue(ctx, time.Date(2026, 11, 3, 2, 30, 0, 0, time.FixedZone("UTC+2", 7200))); err != nil || due {
		t.Fatalf("NightlyDue non-UTC zone = %v, %v; want false (UTC date still 2026-11-03)", due, err)
	}
	// And a local time whose UTC date has already rolled over is due:
	// 23:00 in UTC-1 is 00:00 UTC on the 4th.
	if due, err := s.NightlyDue(ctx, time.Date(2026, 11, 3, 23, 0, 0, 0, time.FixedZone("UTC-1", -3600))); err != nil || !due {
		t.Fatalf("NightlyDue after UTC rollover = %v, %v; want true", due, err)
	}
}

func TestEnsureDefaultsIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.EnsureDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.client.Get(ctx, AutoModeKey).Result(); v != AutoModeOff {
		t.Fatalf("auto_mode = %q", v)
	}
	if v, err := s.client.Get(ctx, NightlyFlagKey).Result(); err != nil || v != "" {
		t.Fatalf("nightly_flag = %q, %v", v, err)
	}
	// Operator state must survive a second call.
	if err := s.SetAutoMode(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetAutoMode(ctx); v != AutoModeOn {
		t.Fatalf("auto_mode clobbered to %q", v)
	}
}

func TestNewRejectsBadAddr(t *testing.T) {
	if _, err := New("not a url"); err == nil {
		t.Fatal("New(bad addr) = nil error")
	}
	if _, err := New("redis://127.0.0.1:1"); err == nil {
		t.Fatal("New(unreachable) = nil error")
	}
}
