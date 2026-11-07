package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/bridge"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

type fakePlan struct{ err error; got queue.Message }

func (f *fakePlan) PublishPlan(ctx context.Context, m queue.Message) (int, error) {
	f.got = m
	return 1, f.err
}

type fakeLoc struct{ err error; loc string; got queue.Message }

func (f *fakeLoc) PublishLocation(ctx context.Context, loc string, m queue.Message) error {
	f.loc = loc
	f.got = m
	return f.err
}

func TestQueueRelayNilFailsWithStoreSettleError(t *testing.T) {
	r := &queueRelay{}
	if _, err := r.PublishPlan(context.Background(), queue.Message{ID: "x"}); err == nil {
		t.Fatal("expected error from empty relay")
	} else if !strings.Contains(err.Error(), "no plan queue publisher") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := r.PublishLocation(context.Background(), "cpu", queue.Message{ID: "x"}); err == nil {
		t.Fatal("expected error from empty relay")
	} else if !strings.Contains(err.Error(), "no location queue publisher") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueueRelaySwapsInPublishers(t *testing.T) {
	plan := &fakePlan{}
	loc := &fakeLoc{}
	r := &queueRelay{}
	r.set(plan, loc)

	m := queue.Message{ID: "j1", Title: "t"}
	if n, err := r.PublishPlan(context.Background(), m); err != nil || n != 1 {
		t.Fatalf("PublishPlan = %d, %v", n, err)
	}
	if plan.got.ID != "j1" {
		t.Fatalf("plan got %q", plan.got.ID)
	}
	if err := r.PublishLocation(context.Background(), "igpu", m); err != nil {
		t.Fatal(err)
	}
	if loc.loc != "igpu" || loc.got.ID != "j1" {
		t.Fatalf("location got loc=%q id=%q", loc.loc, loc.got.ID)
	}

	// A swap replaces what publishes see next.
	plan2 := &fakePlan{err: errors.New("gone")}
	r.set(plan2, loc)
	if _, err := r.PublishPlan(context.Background(), m); err == nil || err.Error() != "gone" {
		t.Fatalf("expected the swapped-in error, got %v", err)
	}
}

func TestQueueRelayImplementsBridgeInterfaces(t *testing.T) {
	var _ bridge.PlanQueuePublisher = (*queueRelay)(nil)
	var _ bridge.LocationQueuePublisher = (*queueRelay)(nil)
}
