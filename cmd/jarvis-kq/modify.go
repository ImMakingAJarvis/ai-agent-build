package main

// modify.go: the jarvis-kq mutation commands (plan §4): pop, seek, replay,
// requeue, edit, dead/dead-requeue/dead-drop, and the group controls
// pause/resume/throttle/rebalance (published to kq.control for the phase-3
// consumer SDK). rollback (AMQP purge to the mirror boundary) is in
// rollback.go.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kq"
	"github.com/segmentio/kafka-go"
)

// cmdPop: pop <group> <topic> [-n 10] — consume up to n messages for the
// group and commit (the Kafka basic.get with requeue=false).
func cmdPop(ctx context.Context, c *kq.Client, args []string) error {
	pos := positional(args)
	if len(pos) != 2 {
		return fmt.Errorf("usage: pop <group> <topic> [-n 10]")
	}
	group, topic := pos[0], pos[1]
	fs := flag.NewFlagSet("pop", flag.ExitOnError)
	n := fs.Int("n", 10, "max messages")
	if err := fs.Parse(flagsAfterPositional(args)); err != nil {
		return err
	}
	msgs, err := c.Pop(ctx, group, topic, *n)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		fmt.Printf("%s: no messages for group %s\n", topic, group)
		return nil
	}
	printRecords(msgs)
	return nil
}

// cmdSeek: seek <group> <topic> (--to N | --to ts | --earliest | --latest)
// [--p 0] — external OffsetCommit (the group need not be running).
func cmdSeek(ctx context.Context, c *kq.Client, args []string) error {
	pos := positional(args)
	if len(pos) != 2 {
		return fmt.Errorf("usage: seek <group> <topic> (--to N | --to ts | --earliest | --latest) [--p 0]")
	}
	group, topic := pos[0], pos[1]
	fs := flag.NewFlagSet("seek", flag.ExitOnError)
	p := fs.Int("p", 0, "partition")
	to := fs.String("to", "", "offset or RFC3339 time")
	earliest := fs.Bool("earliest", false, "first offset")
	latest := fs.Bool("latest", false, "log end (next offset)")
	if err := fs.Parse(flagsAfterPositional(args)); err != nil {
		return err
	}
	var off int64
	var err error
	switch {
	case *earliest:
		off, err = firstOffset(ctx, c, topic)
	case *latest:
		off, err = lastOffset(ctx, c, topic)
	case *to == "":
		return fmt.Errorf("one of --to, --earliest, --latest is required")
	default:
		off, err = resolveOffset(ctx, c, topic, *to)
	}
	if err != nil {
		return err
	}
	before := committedOffset(ctx, c, group, topic, *p)
	if err := c.Seek(ctx, group, topic, *p, off); err != nil {
		return err
	}
	fmt.Printf("%s/%s p%d: committed offset %s -> %d\n", group, topic, *p, before, off)
	return nil
}

// committedOffset renders the group's current commit for one partition
// ("none" while uncommitted) for the before/after report.
func committedOffset(ctx context.Context, c *kq.Client, group, topic string, partition int) string {
	off, err := c.GroupOffset(ctx, group)
	if err != nil {
		return "unknown"
	}
	for _, p := range off[topic] {
		if p.Partition == partition {
			return strconv.FormatInt(p.CommittedOffset, 10)
		}
	}
	return "none"
}

func firstOffset(ctx context.Context, c *kq.Client, topic string) (int64, error) {
	f, err := c.FirstOffsets(ctx, topic)
	if err != nil {
		return 0, err
	}
	return f[0], nil
}

func lastOffset(ctx context.Context, c *kq.Client, topic string) (int64, error) {
	l, err := c.LastOffsets(ctx, topic)
	if err != nil {
		return 0, err
	}
	return l[0], nil
}

// cmdReplay: replay <topic> [--from off|ts] [--to off|ts] [--to-topic T]
// [-n 500] — read the slice [from, to) and republish it (default: to the
// same topic), marked with kq-replayed.
func cmdReplay(ctx context.Context, c *kq.Client, args []string) error {
	pos := positional(args)
	if len(pos) != 1 {
		return fmt.Errorf("usage: replay <topic> [--from off|ts] [--to off|ts] [--to-topic T] [-n 500]")
	}
	topic := pos[0]
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	from := fs.String("from", "", "offset or RFC3339 time (default: first)")
	to := fs.String("to", "", "offset or RFC3339 time (default: log end)")
	dest := fs.String("to-topic", "", "republish destination (default: same topic)")
	n := fs.Int("n", 500, "max records")
	if err := fs.Parse(flagsAfterPositional(args)); err != nil {
		return err
	}
	fromOff, err := resolveOffsetOr(ctx, c, topic, *from, true)
	if err != nil {
		return err
	}
	toOff, err := resolveOffsetOr(ctx, c, topic, *to, false)
	if err != nil {
		return err
	}
	msgs, err := c.ReadRange(ctx, topic, 0, fromOff, toOff, *n)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		fmt.Printf("%s: no records in [%d, %d)\n", topic, fromOff, toOff)
		return nil
	}
	if *dest == "" {
		*dest = topic
	}
	var lastPart int
	var lastOff int64
	for i := range msgs {
		p, off, err := c.Republish(ctx, *dest, &msgs[i], kafka.Header{
			Key: "kq-replayed", Value: []byte(time.Now().UTC().Format(time.RFC3339)),
		})
		if err != nil {
			return fmt.Errorf("record %d: %w", msgs[i].Offset, err)
		}
		lastPart, lastOff = p, off
	}
	fmt.Printf("%s: replayed %d record(s) from [%d, %d) to %s p%d (last copy at offset %d)\n",
		topic, len(msgs), fromOff, toOff, *dest, lastPart, lastOff)
	return nil
}

// resolveOffsetOr turns "N" / RFC3339 into an offset; empty s falls back to
// the first (fromEnd=true) or last (log end) offset.
func resolveOffsetOr(ctx context.Context, c *kq.Client, topic, s string, fromEnd bool) (int64, error) {
	if s == "" {
		if fromEnd {
			return firstOffset(ctx, c, topic)
		}
		return lastOffset(ctx, c, topic)
	}
	return resolveOffset(ctx, c, topic, s)
}

// resolveOffset turns "N" or an RFC3339 time into an absolute offset.
func resolveOffset(ctx context.Context, c *kq.Client, topic, s string) (int64, error) {
	if off, err := strconv.ParseInt(s, 10, 64); err == nil {
		return off, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, fmt.Errorf("%q is neither an offset nor RFC3339", s)
	}
	at, err := c.OffsetAt(ctx, topic, t)
	if err != nil {
		return 0, err
	}
	return at[0], nil
}

// cmdRequeue: requeue <topic> <offset|corr> [--to-topic T] — republish one
// record (matched by offset, or by key / body id) keeping key and headers.
func cmdRequeue(ctx context.Context, c *kq.Client, args []string) error {
	pos := positional(args)
	if len(pos) != 2 {
		return fmt.Errorf("usage: requeue <topic> <offset|corr> [--to-topic T]")
	}
	topic, ref := pos[0], pos[1]
	fs := flag.NewFlagSet("requeue", flag.ExitOnError)
	dest := fs.String("to-topic", "", "republish destination (default: same topic)")
	if err := fs.Parse(flagsAfterPositional(args)); err != nil {
		return err
	}
	var (
		msg *kafka.Message
		err error
	)
	if off, perr := strconv.ParseInt(ref, 10, 64); perr == nil {
		msg, err = c.ReadAt(ctx, topic, 0, off)
	} else {
		msg, err = c.FindByCorrelation(ctx, topic, ref, 1000)
	}
	if err != nil {
		return err
	}
	if *dest == "" {
		*dest = msg.Topic
	}
	p, newOff, err := c.Republish(ctx, *dest, msg, kafka.Header{
		Key: "kq-requeued", Value: []byte(time.Now().UTC().Format(time.RFC3339)),
	})
	if err != nil {
		return err
	}
	fmt.Printf("%s offset %d: requeued to %s p%d/%d\n", msg.Topic, msg.Offset, *dest, p, newOff)
	return nil
}

// cmdEdit: edit <topic> <offset> --set k=v ... — read the record, set the
// keys on its JSON object body, republish with kq-edited markers. The
// original stays (append-only); the copy is the fixed version.
func cmdEdit(ctx context.Context, c *kq.Client, args []string) error {
	pos := positional(args)
	if len(pos) != 2 {
		return fmt.Errorf("usage: edit <topic> <offset> --set k=v [--set k2=v2 ...] [--to-topic T]")
	}
	topic, ref := pos[0], pos[1]
	fs := flag.NewFlagSet("edit", flag.ExitOnError)
	var sets kvFlags
	fs.Var(&sets, "set", "key=value to set in the JSON body (repeatable)")
	dest := fs.String("to-topic", "", "republish destination (default: same topic)")
	if err := fs.Parse(flagsAfterPositional(args)); err != nil {
		return err
	}
	if len(sets) == 0 {
		return fmt.Errorf("at least one --set k=v is required")
	}
	off, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return fmt.Errorf("offset must be numeric (got %q)", ref)
	}
	msg, err := c.ReadAt(ctx, topic, 0, off)
	if err != nil {
		return err
	}
	var body map[string]any
	if err := json.Unmarshal(msg.Value, &body); err != nil {
		return fmt.Errorf("record %d body is not a JSON object: %w", off, err)
	}
	for _, kv := range sets {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			return fmt.Errorf("--set needs key=value (got %q)", kv)
		}
		key, val := kv[:eq], kv[eq+1:]
		var jv any
		if err := json.Unmarshal([]byte(val), &jv); err == nil {
			body[key] = jv // numbers/bools/objects keep their type
		} else {
			body[key] = val
		}
	}
	out, err := json.Marshal(body)
	if err != nil {
		return err
	}
	msg.Value = out
	if *dest == "" {
		*dest = msg.Topic
	}
	p, newOff, err := c.Republish(ctx, *dest, msg,
		kafka.Header{Key: "kq-edited", Value: []byte("true")},
		kafka.Header{Key: "kq-edit-of", Value: []byte(strconv.FormatInt(off, 10))})
	if err != nil {
		return err
	}
	fmt.Printf("%s offset %d: edited copy at %s p%d/%d (%s)\n",
		msg.Topic, off, *dest, p, newOff, string(out))
	return nil
}

// cmdDead: dead [-n 20] [--all] — list dead records (oldest first); --all
// includes the ones marked dropped.
func cmdDead(ctx context.Context, c *kq.Client, args []string) error {
	fs := flag.NewFlagSet("dead", flag.ExitOnError)
	n := fs.Int("n", 20, "max records")
	all := fs.Bool("all", false, "include dropped records")
	if err := fs.Parse(args); err != nil {
		return err
	}
	items, err := c.ListDead(ctx, *n, *all)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Println("no dead records")
		return nil
	}
	t := newTab()
	t.WriteLine("OFFSET", "REASON", "QUEUE", "ATTEMPTS", "DEAD-AT", "VALUE")
	for _, d := range items {
		t.WriteLine(strconv.FormatInt(d.Offset, 10), d.Reason, d.Queue,
			strconv.Itoa(d.Attempts), d.At, clip(d.Value, 80))
	}
	return t.Flush()
}

// cmdDeadRequeue: dead-requeue <offset> — republish to the original queue
// (recreated if gone) and mark the dead record handled.
func cmdDeadRequeue(ctx context.Context, c *kq.Client, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: dead-requeue <offset>")
	}
	off, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("offset must be numeric (got %q)", args[0])
	}
	queue, p, newOff, err := c.RequeueDead(ctx, off)
	if err != nil {
		return err
	}
	fmt.Printf("dead offset %d: requeued to %s p%d/%d\n", off, queue, p, newOff)
	return nil
}

// cmdDeadDrop: dead-drop <offset> — mark as dropped (stays in the log; the
// marker keeps it out of the pending lists).
func cmdDeadDrop(ctx context.Context, c *kq.Client, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: dead-drop <offset>")
	}
	off, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("offset must be numeric (got %q)", args[0])
	}
	if err := c.DropDead(ctx, off); err != nil {
		return err
	}
	fmt.Printf("dead offset %d: dropped (marked, log kept)\n", off)
	return nil
}

// cmdControl: pause/resume <group> [topic], throttle <group> <rate> [topic],
// rebalance <group> [topic] — publish a control record to kq.control; the
// phase-3 consumer SDK applies it on every poll (and it survives restarts,
// being a log record).
func cmdControl(ctx context.Context, c *kq.Client, args []string, action string, needsRate bool) error {
	var min int
	if needsRate {
		min = 2
	} else {
		min = 1
	}
	if len(args) < min || len(args) > min+1 {
		usage := "pause|resume|rebalance <group> [topic]"
		if needsRate {
			usage = "throttle <group> <rate> [topic]"
		}
		return fmt.Errorf("usage: %s", usage)
	}
	group := args[0]
	a := kq.ControlAction{Action: action}
	if needsRate {
		rate, err := strconv.ParseFloat(args[1], 64)
		if err != nil || rate <= 0 {
			return fmt.Errorf("rate must be a positive number of messages/s (got %q)", args[1])
		}
		a.Rate = rate
		if len(args) > 2 {
			a.Topic = args[2]
		}
	} else if len(args) > 1 {
		a.Topic = args[1]
	}
	if action == "rebalance" {
		if st, err := c.DescribeGroup(ctx, group); err == nil {
			fmt.Printf("group %s: %s, %d member(s) before the rebalance\n", st.GroupID, st.GroupState, st.Members)
		}
	}
	if err := c.PublishControl(ctx, group, a); err != nil {
		return err
	}
	fmt.Printf("kq.control: %s for group %s (topic %s) — the consumer SDK applies it on its next poll\n",
		action, group, orDash(a.Topic))
	return nil
}

// positional returns the leading non-flag arguments of args.
func positional(args []string) []string {
	var pos []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		pos = append(pos, a)
	}
	return pos
}

// flagsAfterPositional returns args minus the leading positional run — the
// flag set for fs.Parse (flags come after the positionals, like splitPos).
func flagsAfterPositional(args []string) []string {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			return args[i:]
		}
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
