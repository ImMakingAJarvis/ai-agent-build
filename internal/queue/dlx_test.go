package queue

import (
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// TestDLXArgs pins the dead-lettering argument every cap.*/flow.* queue
// is declared with (EVB-10): the jarvis.dlx fanout exchange.
func TestDLXArgs(t *testing.T) {
	args := dlxArgs()
	if got, _ := args["x-dead-letter-exchange"].(string); got != DLXExchange {
		t.Errorf("x-dead-letter-exchange = %q, want %q", got, DLXExchange)
	}
	if len(args) != 1 {
		t.Errorf("dlxArgs = %v, want only x-dead-letter-exchange", args)
	}
}

func TestAttemptCountAndBump(t *testing.T) {
	d := amqp.Delivery{}
	if got := AttemptCount(d); got != 0 {
		t.Errorf("first delivery AttemptCount = %d, want 0", got)
	}
	h := BumpAttempt(d)
	if got, _ := h[AttemptHeader]; toIntVal(got) != 1 {
		t.Errorf("bump from 0 = %v, want 1", got)
	}
	d.Headers = h
	h = BumpAttempt(d)
	if got, _ := h[AttemptHeader]; toIntVal(got) != 2 {
		t.Errorf("bump from 1 = %v, want 2", got)
	}
	// Other headers survive the bump.
	d.Headers = amqp.Table{"x-other": "keep", AttemptHeader: int32(2)}
	h = BumpAttempt(d)
	if h["x-other"] != "keep" {
		t.Errorf("bump dropped other headers: %v", h)
	}
}

func toIntVal(v any) int {
	n, _ := toInt(v)
	return n
}

func TestParseXDeath(t *testing.T) {
	// Classic shape: a list of [count, reason, exchange, queue,
	// time-unix] entries, newest first.
	classic := amqp.Table{
		"x-death": []any{
			[]any{int32(2), "rejected", "jarvis.dlx", "flow.reel", int64(1760000000)},
			[]any{int32(1), "rejected", "jarvis.dlx", "flow.reel", int64(1759999000)},
		},
	}
	got := parseXDeath(classic)
	if len(got) != 2 {
		t.Fatalf("classic: parsed %d deaths, want 2", len(got))
	}
	if got[0].Count != 2 || got[0].Reason != "rejected" || got[0].Queue != "flow.reel" {
		t.Errorf("classic death[0] = %+v", got[0])
	}
	if !got[0].Time.Equal(time.Unix(1760000000, 0).UTC()) {
		t.Errorf("classic death[0].Time = %v, want %v", got[0].Time, time.Unix(1760000000, 0).UTC())
	}

	// Table shape (RabbitMQ 3.13+): a list of tables, time as an AMQP
	// timestamp.
	tables := amqp.Table{
		"x-death": []any{
			amqp.Table{
				"count":        int32(1),
				"reason":       "rejected",
				"exchange":     "tasks",
				"queue":        "flow.reel",
				"time":         time.Date(2026, 11, 8, 16, 56, 49, 0, time.UTC),
				"routing-keys": []any{"flow.reel.default"},
			},
		},
	}
	got = parseXDeath(tables)
	if len(got) != 1 {
		t.Fatalf("tables: parsed %d deaths, want 1", len(got))
	}
	if got[0].Count != 1 || got[0].Reason != "rejected" || got[0].Queue != "flow.reel" {
		t.Errorf("tables death[0] = %+v", got[0])
	}
	if !got[0].Time.Equal(time.Date(2026, 11, 8, 16, 56, 49, 0, time.UTC)) {
		t.Errorf("tables death[0].Time = %v", got[0].Time)
	}

	// A header of the wrong shape is ignored, not fatal.
	if got := parseXDeath(amqp.Table{"x-death": "nope"}); len(got) != 0 {
		t.Errorf("string x-death parsed as %v, want none", got)
	}
}

func TestFirstDeath(t *testing.T) {
	// No x-death header at all (published straight onto the dead queue):
	// the dashboard still has to show it.
	del := amqp.Delivery{Headers: amqp.Table{}}
	if d := firstDeath(del); d.Reason != "unknown" {
		t.Errorf("no-header reason = %q, want unknown", d.Reason)
	}
	del.Headers = amqp.Table{
		"x-death": []any{
			[]any{int32(1), "expired", "jarvis.dlx", "cap.transcribe", int64(1760000000)},
		},
	}
	if d := firstDeath(del); d.Reason != "expired" || d.Queue != "cap.transcribe" {
		t.Errorf("firstDeath = %+v", d)
	}
}
