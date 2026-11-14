//go:build integration

// Broker-backed integration tests for the mailbox wake path (plan §3.8,
// task EVB-35): a question with a wake_job is a listener-wake — the answer
// lands on the QUESTIONER's inbox carrying the question's corr, the
// switchboard's corr-filtered wake listener on that inbox sees it, and the
// parked job wakes within 5 s. The mailbox queues are durable with the
// dead-letter args, so the pair also survives a broker restart.
//
// Run with:  go test -tags integration ./internal/switchboard/

package switchboard

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// publishMail publishes a mailbox message the way jarvis-mq ask/tell/answer
// does: through PublishMailbox (queue created on demand, confirm mode,
// persistent delivery).
func publishMail(t *testing.T, conn *queue.Conn, m queue.MailboxMessage) {
	t.Helper()
	if err := conn.PublishMailbox(context.Background(), m); err != nil {
		t.Fatalf("publish mailbox %s -> %s: %v", m.From, m.To, err)
	}
}

// mailByCorr finds the message with corr in a peeked inbox, or nil. The
// mailbox tests share one broker (and one agent.a/agent.b inbox pair), so
// an inbox may legitimately hold mail from another test — assertions match
// on the corr, never on the inbox size.
func mailByCorr(mails []queue.MailboxMessage, corr string) *queue.MailboxMessage {
	for i := range mails {
		if mails[i].Corr == corr {
			return &mails[i]
		}
	}
	return nil
}

// TestIntegrationMailboxWake is the mailbox acceptance criterion: a
// question's wake fires when the ANSWER lands — on the questioner's inbox,
// filtered on the question's corr — and the job wakes within 5 s.
func TestIntegrationMailboxWake(t *testing.T) {
	getBroker(t)
	jobs := newJobs(t)
	ls, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open listeners: %v", err)
	}
	m, _ := NewMetrics()

	conn := connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := New(Config{Conn: conn, Jobs: jobs, Listen: ls, M: m, PollInterval: 100 * time.Millisecond})
	go sb.Run(ctx)

	addRunning(t, jobs, "mb-1")
	yield(t, jobs, "mb-1", []string{"q-mb"}, time.Minute)
	// The listener watches the QUESTIONER's (agent a's) inbox, because that
	// is where the answer lands — mirroring what jarvis-mq ask --wake does.
	if err := ls.Create(Listener{ID: "mb-1", Queue: queue.MailboxQueueName("a"), Action: ActionWake, JobID: "mb-1", CorrFilter: "q-mb"}); err != nil {
		t.Fatalf("create listener: %v", err)
	}

	// The question goes to b's inbox; b never acts on it (the switchboard
	// has no listener there), it just sits for b's round to read.
	publishMail(t, conn, queue.MailboxMessage{
		ID: "mbq-1", Kind: queue.MailKindQuestion, From: "a", To: "b",
		Subject: "how hot is the engine", Body: "before the photo window",
		Corr: "q-mb", WakeJob: "mb-1", CreatedAt: time.Now().UTC(),
	})
	// The answer goes to a's inbox carrying the same corr — this is the wake.
	publishMail(t, conn, queue.MailboxMessage{
		ID: "mba-1", Kind: queue.MailKindAnswer, From: "b", To: "a",
		Subject: "engine at 61C, fine", Body: "vram headroom ok",
		Corr: "q-mb", CreatedAt: time.Now().UTC(),
	})

	waitFor(t, 5*time.Second, "the job to wake on the mailbox answer", func() bool {
		j, err := jobs.Get("mb-1")
		return err == nil && j.Status == agentjob.Ready && testutil.ToFloat64(m.WakesTotal) == 1
	})
	rec, err := jobs.ReadWake("mb-1")
	if err != nil {
		t.Fatalf("read wake: %v", err)
	}
	if rec.Reason != agentjob.WakeAll {
		t.Fatalf("wake reason = %s, want %s", rec.Reason, agentjob.WakeAll)
	}
	if ref := rec.Results["q-mb"]; ref != "engine at 61C, fine" {
		t.Fatalf("wake ref for q-mb = %q, want the answer's subject", ref)
	}

	// The question is untouched on b's inbox: the wake only acts where the
	// listener is, and a wake always leaves its message for the round.
	qs, err := conn.PeekMailbox("b", 5)
	if err != nil {
		t.Fatalf("peek b: %v", err)
	}
	if q := mailByCorr(qs, "q-mb"); q == nil || q.Kind != queue.MailKindQuestion {
		t.Fatalf("b's inbox = %+v, want the question (corr q-mb) still there", qs)
	}
	as, err := conn.PeekMailbox("a", 5)
	if err != nil {
		t.Fatalf("peek a: %v", err)
	}
	if a := mailByCorr(as, "q-mb"); a == nil || a.Kind != queue.MailKindAnswer {
		t.Fatalf("a's inbox = %+v, want the answer (corr q-mb) kept for the woken round", as)
	}
}

// TestIntegrationMailboxDurability is the durability acceptance criterion:
// a question posted before a broker restart is still on the recipient's
// inbox afterwards (durable queue, persistent delivery), and the wake path
// still completes — the listener is on disk, the answer republished after
// the restart wakes the job.
func TestIntegrationMailboxDurability(t *testing.T) {
	b := getBroker(t)
	jobs := newJobs(t)
	ls, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open listeners: %v", err)
	}
	m, _ := NewMetrics()

	addRunning(t, jobs, "mb-dur")
	yield(t, jobs, "mb-dur", []string{"q-dur"}, time.Minute)
	if err := ls.Create(Listener{ID: "mb-dur", Queue: queue.MailboxQueueName("a"), Action: ActionWake, JobID: "mb-dur", CorrFilter: "q-dur"}); err != nil {
		t.Fatalf("create listener: %v", err)
	}

	// The question is published while the first broker is up...
	conn1 := connect(t)
	publishMail(t, conn1, queue.MailboxMessage{
		ID: "mbq-dur", Kind: queue.MailKindQuestion, From: "a", To: "b",
		Subject: "check the photo window", Body: "12GB headroom needed",
		Corr: "q-dur", WakeJob: "mb-dur", CreatedAt: time.Now().UTC(),
	})
	conn1.Close()
	b.restart(t) // docker stop + start: durable queue and message come back

	// ...and the question is still there afterwards (matched by corr —
	// the inbox may hold mail from the wake test on the same broker).
	conn2 := connect(t)
	qs, err := conn2.PeekMailbox("b", 5)
	if err != nil {
		t.Fatalf("peek after restart: %v", err)
	}
	q := mailByCorr(qs, "q-dur")
	if q == nil || q.Subject != "check the photo window" || q.From != "a" || q.To != "b" {
		t.Fatalf("post-restart inbox = %+v, want the surviving question (corr q-dur)", qs)
	}

	// The answer lands on the restarted broker and the job still wakes
	// (listener re-read from disk, queue reconnected).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := New(Config{Conn: conn2, Jobs: jobs, Listen: ls, M: m, PollInterval: 100 * time.Millisecond})
	go sb.Run(ctx)
	publishMail(t, conn2, queue.MailboxMessage{
		ID: "mba-dur", Kind: queue.MailKindAnswer, From: "b", To: "a",
		Subject: "window opened, 13GB free", Body: "procedural, no brain needed",
		Corr: "q-dur", CreatedAt: time.Now().UTC(),
	})
	waitFor(t, 5*time.Second, "the job to wake after the broker restart", func() bool {
		j, err := jobs.Get("mb-dur")
		return err == nil && j.Status == agentjob.Ready
	})
}
