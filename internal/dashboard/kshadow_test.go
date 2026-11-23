package dashboard

import (
	"testing"
	"time"

	ks "github.com/ImMakingAJarvis/ai-agent-build/internal/kshadow"
)

func TestKshadowStatus(t *testing.T) {
	now := time.Now().UTC()

	if st, cls := kshadowStatus(nil); st != "down" || cls != "bad" {
		t.Errorf("nil: %s/%s", st, cls)
	}
	if st, cls := kshadowStatus(ks.KshadowDown()); st != "down" || cls != "bad" {
		t.Errorf("down: %s/%s", st, cls)
	}

	ok := &ks.Report{Kshadow: "up", At: now, Rows: []ks.Row{
		{Queue: "voice", Published: 10, Mirrored: 10, Written: 10, Consumed: 9, Dropped: 0, Lag: "2s", LastAt: now.Add(-2 * time.Second)},
	}}
	if st, cls := kshadowStatus(ok); st != "ok" || cls != "good" {
		t.Errorf("ok: %s/%s", st, cls)
	}

	mis := &ks.Report{Kshadow: "up", Rows: []ks.Row{
		{Queue: "voice", Dropped: 3},
	}}
	if st, cls := kshadowStatus(mis); st != "mismatch" || cls != "bad" {
		t.Errorf("dropped: %s/%s", st, cls)
	}
	mis2 := &ks.Report{Kshadow: "up", Rows: []ks.Row{
		{Queue: "voice", Mismatch: 1},
	}}
	if st, cls := kshadowStatus(mis2); st != "mismatch" || cls != "bad" {
		t.Errorf("mismatch: %s/%s", st, cls)
	}

	// lag is judged on the events aggregate (the continuously active
	// queue) — a bursty queue 6 min from its last message is idle, not
	// a stalled mirror
	lag := &ks.Report{Kshadow: "up", Rows: []ks.Row{
		{Queue: "events", Lag: "6m0s", LastAt: now.Add(-6 * time.Minute)},
	}}
	if st, cls := kshadowStatus(lag); st != "lag" || cls != "warn" {
		t.Errorf("lag: %s/%s", st, cls)
	}
	quiet := &ks.Report{Kshadow: "up", Rows: []ks.Row{
		{Queue: "voice", Published: 4, Mirrored: 4, Written: 4, Lag: "6m0s", LastAt: now.Add(-6 * time.Minute)},
	}}
	if st, cls := kshadowStatus(quiet); st != "ok" || cls != "good" {
		t.Errorf("quiet bursty queue: %s/%s", st, cls)
	}
	// an idle queue (never seen) is not lagging
	idle := &ks.Report{Kshadow: "up", Rows: []ks.Row{{Queue: "dead"}}}
	if st, cls := kshadowStatus(idle); st != "ok" || cls != "good" {
		t.Errorf("idle: %s/%s", st, cls)
	}
}
