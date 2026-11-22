// jarvis-kq is the Kafka queue tool for the RabbitMQ→Kafka shadow
// migration (docs/kafka-migration-plan.md §4): inspect topics, offsets,
// consumer groups and records, and do the basic modifications (send,
// purge, drop). It is the Kafka counterpart of the queue subcommands in
// jarvis-mq; the AMQP side stays on jarvis-mq.
//
//	Topics/offsets:  topics, depth, describe
//	Records:         peek, tail, history
//	Groups:          groups, lag
//	Modify:          send, purge, drop
//
// The broker list comes from KAFKA_BROKERS (default 127.0.0.1:9092).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kq"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/kshadow"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, rest := os.Args[1], os.Args[2:]
	brokers := envOr("KAFKA_BROKERS", "127.0.0.1:9092")
	c := kq.New(strings.Split(brokers, ",")...)
	ctx := context.Background()
	var err error
	switch cmd {
	case "topics":
		err = cmdTopics(ctx, c)
	case "depth":
		err = cmdDepth(ctx, c, rest)
	case "peek":
		err = cmdPeek(ctx, c, rest, false)
	case "tail":
		err = cmdPeek(ctx, c, rest, true)
	case "groups":
		err = cmdGroups(ctx, c)
	case "lag":
		err = cmdLag(ctx, c, rest)
	case "history":
		err = cmdHistory(ctx, c, rest)
	case "describe":
		err = cmdDescribe(ctx, c, rest)
	case "send":
		err = cmdSend(ctx, c, rest)
	case "purge":
		err = cmdPurge(ctx, c, rest)
	case "drop":
		err = cmdDrop(ctx, c, rest)
	case "parity":
		os.Exit(cmdParity(ctx, c, rest))
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "jarvis-kq: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "jarvis-kq:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: jarvis-kq <command> [args]

  topics                          list topics (name, partitions, leader)
  depth <topic>                   per-partition first/last offset and depth
  peek <topic> [-n 10] [-p 0] [--offset N]
                                  records from N (absolute) or the last N
  tail <topic> [-n 10] [-p 0]     the last N records (alias for peek)
  groups                          list consumer groups
  lag <group> [topic...]          committed vs last per partition
  history <topic> --since 2h [-n 50]
                                  records since a duration or RFC3339 time
  describe <topic>                partitions, leaders, retention, config
  send <topic> (--body S | --payload k=v ...) [--key K] [--priority N]
  purge <topic>                   delete all records in the topic
  drop <topic>                    delete the topic itself
  parity [--max-lag 5m] [--json]  AMQP vs Kafka shadow parity per queue
                                  (exit 1 = mismatch, 2 = lag, 3 = kshadow down)

environment: KAFKA_BROKERS (default 127.0.0.1:9092)
           : RABBITMQ_URL, KSHADOW_STATE (kshadow's state file, for parity)
`)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// tab is a tabwriter with a WriteLine helper (the stdlib type has none).
type tab struct {
	*tabwriter.Writer
}

func newTab() *tab {
	return &tab{tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)}
}

func (t *tab) WriteLine(cols ...string) {
	fmt.Fprintln(t.Writer, strings.Join(cols, "\t"))
}

// clip shortens s to n bytes with an ellipsis.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// parseSince accepts a Go duration ("2h") relative to now, or RFC3339.
func parseSince(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("cannot parse %q as a duration or RFC3339 time", s)
}

func cmdTopics(ctx context.Context, c *kq.Client) error {
	topics, err := c.Topics(ctx)
	if err != nil {
		return err
	}
	sort.Slice(topics, func(i, j int) bool { return topics[i].Name < topics[j].Name })
	w := newTab()
	w.WriteLine("TOPIC", "PARTITIONS", "LEADER")
	for _, t := range topics {
		leader := "-"
		if len(t.Partitions) > 0 && t.Partitions[0].Leader.ID >= 0 {
			leader = strconv.Itoa(t.Partitions[0].Leader.ID)
		}
		w.WriteLine(t.Name, strconv.Itoa(len(t.Partitions)), leader)
	}
	return w.Flush()
}

func cmdDepth(ctx context.Context, c *kq.Client, args []string) error {
	fs := flag.NewFlagSet("depth", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: depth <topic>")
	}
	topic := fs.Arg(0)
	first, last, total, err := depthOf(ctx, c, topic)
	if err != nil {
		return err
	}
	w := newTab()
	w.WriteLine("PARTITION", "FIRST", "LAST", "DEPTH")
	for _, p := range sortedKeys(first) {
		w.WriteLine(strconv.Itoa(p),
			strconv.FormatInt(first[p], 10),
			strconv.FormatInt(last[p], 10),
			strconv.FormatInt(last[p]-first[p], 10))
	}
	fmt.Fprintf(w, "TOTAL\t%d\t%d\t%d\n", len(first), sumOf(last), total)
	return w.Flush()
}

// depthOf returns per-partition first/last offsets and the total depth.
func depthOf(ctx context.Context, c *kq.Client, topic string) (map[int]int64, map[int]int64, int64, error) {
	first, err := c.FirstOffsets(ctx, topic)
	if err != nil {
		return nil, nil, 0, err
	}
	last, err := c.LastOffsets(ctx, topic)
	if err != nil {
		return nil, nil, 0, err
	}
	var total int64
	for p := range first {
		total += last[p] - first[p]
	}
	return first, last, total, nil
}

func sumOf(m map[int]int64) int64 {
	var v int64
	for _, x := range m {
		v += x
	}
	return v
}

func sortedKeys(m map[int]int64) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// splitPos pulls the first non-flag argument (the topic or group) out of a
// flag+positional list and returns the remaining flags: Go's flag package
// stops parsing at the first positional, so flags written after it would
// otherwise be silently ignored.
func splitPos(args []string) (string, []string, bool) {
	pos := -1
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if i > 0 && strings.HasPrefix(args[i-1], "-") {
			continue // value of the preceding flag
		}
		pos = i
		break
	}
	if pos < 0 {
		return "", nil, false
	}
	rest := append(append([]string{}, args[:pos]...), args[pos+1:]...)
	return args[pos], rest, true
}

// cmdPeek prints records from a partition: from --offset when given,
// otherwise the last N (fromEnd = the tail alias).
func cmdPeek(ctx context.Context, c *kq.Client, args []string, fromEnd bool) error {
	fs := flag.NewFlagSet("peek", flag.ExitOnError)
	n := fs.Int("n", 10, "number of records")
	p := fs.Int("p", 0, "partition")
	offset := fs.Int64("offset", 0, "start offset (absolute; implies reading forward)")
	topic, rest, ok := splitPos(args)
	fs.Parse(rest)
	if !ok {
		return errors.New("usage: peek <topic> [-n 10] [-p 0] [--offset N]")
	}
	end, isAbs, err := peekStart(ctx, c, topic, *p, *offset)
	if err != nil {
		return err
	}
	start := end
	if !isAbs {
		// end is the log end; back off by n records for the last-N read.
		start -= int64(*n)
	}
	msgs, err := readRecords(c, topic, *p, *n, start)
	if err != nil {
		return err
	}
	printRecords(msgs)
	return nil
}

// peekStart resolves the anchor: --offset when set, otherwise the log-end
// offset of the partition.
func peekStart(ctx context.Context, c *kq.Client, topic string, partition int, offset int64) (int64, bool, error) {
	if offset != 0 {
		return offset, true, nil
	}
	last, err := c.LastOffsets(ctx, topic)
	if err != nil {
		return 0, false, err
	}
	v, ok := last[partition]
	if !ok {
		return 0, false, fmt.Errorf("partition %d not found in topic %q", partition, topic)
	}
	return v, false, nil
}

// readRecords reads up to n records of a partition starting at the
// absolute offset start (clamped to the log range).
func readRecords(c *kq.Client, topic string, partition int, n int, start int64) ([]kafka.Message, error) {
	conn, err := c.Conn(context.Background(), topic, partition)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	first, err := conn.ReadFirstOffset()
	if err != nil {
		return nil, err
	}
	last, err := conn.ReadLastOffset()
	if err != nil {
		return nil, err
	}
	if start < first {
		start = first
	}
	if start >= last {
		return nil, nil
	}
	if _, err := conn.Seek(start, kafka.SeekAbsolute); err != nil {
		return nil, err
	}
	// ReadMessage blocks at the end of the log (it waits for new records,
	// it does not EOF), so never ask for more than exists.
	want := int64(n)
	if last-start < want {
		want = last - start
	}
	var out []kafka.Message
	for int64(len(out)) < want {
		m, err := conn.ReadMessage(1 << 20)
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
		out = append(out, m)
	}
	return out, nil
}

func printRecords(msgs []kafka.Message) {
	if len(msgs) == 0 {
		fmt.Println("(no records)")
		return
	}
	w := newTab()
	w.WriteLine("OFFSET", "TIME", "KEY", "HEADERS", "VALUE")
	for _, m := range msgs {
		hdrs := make([]string, 0, len(m.Headers))
		for _, h := range m.Headers {
			hdrs = append(hdrs, h.Key+"="+clip(string(h.Value), 40))
		}
		w.WriteLine(
			strconv.FormatInt(m.Offset, 10),
			m.Time.Local().Format("2006-01-02 15:04:05"),
			clip(string(m.Key), 40),
			strings.Join(hdrs, ","),
			clip(string(m.Value), 200),
		)
	}
	w.Flush()
}

func cmdGroups(ctx context.Context, c *kq.Client) error {
	groups, err := c.Groups(ctx)
	if err != nil {
		return err
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].GroupID < groups[j].GroupID })
	w := newTab()
	w.WriteLine("GROUP", "COORDINATOR", "PROTOCOL")
	for _, g := range groups {
		w.WriteLine(g.GroupID, strconv.Itoa(g.Coordinator), g.ProtocolType)
	}
	w.Flush()
	return nil
}

func cmdLag(ctx context.Context, c *kq.Client, args []string) error {
	fs := flag.NewFlagSet("lag", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() < 1 {
		return errors.New("usage: lag <group> [topic...]")
	}
	group := fs.Arg(0)
	committed, err := c.GroupOffset(ctx, group)
	if err != nil {
		return err
	}
	topics := fs.Args()[1:]
	if len(topics) == 0 {
		for t := range committed {
			topics = append(topics, t)
		}
		sort.Strings(topics)
	}
	w := newTab()
	w.WriteLine("TOPIC", "PARTITION", "COMMITTED", "LAST", "LAG")
	for _, topic := range topics {
		last, err := c.LastOffsets(ctx, topic)
		if err != nil {
			return err
		}
		byPart := map[int]int64{}
		for _, po := range committed[topic] {
			byPart[po.Partition] = po.CommittedOffset
		}
		for _, p := range sortedKeys(last) {
			comm := byPart[p] // 0 when the group has nothing committed
			w.WriteLine(topic, strconv.Itoa(p),
				strconv.FormatInt(comm, 10),
				strconv.FormatInt(last[p], 10),
				strconv.FormatInt(last[p]-comm, 10))
		}
	}
	return w.Flush()
}

func cmdHistory(ctx context.Context, c *kq.Client, args []string) error {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	since := fs.String("since", "", "duration (2h) or RFC3339 time")
	n := fs.Int("n", 50, "max records per partition")
	topic, rest, ok := splitPos(args)
	fs.Parse(rest)
	if !ok || *since == "" {
		return errors.New("usage: history <topic> --since 2h [-n 50]")
	}
	t, err := parseSince(*since)
	if err != nil {
		return err
	}
	starts, err := c.OffsetAt(ctx, topic, t)
	if err != nil {
		return err
	}
	for _, p := range sortedKeys(starts) {
		msgs, err := readRecords(c, topic, p, *n, starts[p])
		if err != nil {
			return err
		}
		printRecords(msgs)
	}
	return nil
}

func cmdDescribe(ctx context.Context, c *kq.Client, args []string) error {
	fs := flag.NewFlagSet("describe", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: describe <topic>")
	}
	topic := fs.Arg(0)
	md, err := c.Metadata(ctx, []string{topic})
	if err != nil {
		return err
	}
	if len(md.Topics) == 0 {
		return fmt.Errorf("topic %q not found", topic)
	}
	t := md.Topics[0]
	if t.Error != nil {
		return t.Error
	}
	first, err := c.FirstOffsets(ctx, topic)
	if err != nil {
		return err
	}
	last, err := c.LastOffsets(ctx, topic)
	if err != nil {
		return err
	}
	w := newTab()
	w.WriteLine("PARTITION", "LEADER", "ISR", "FIRST", "LAST", "FIRST RECORD", "LAST RECORD")
	for _, p := range t.Partitions {
		isr := make([]string, 0, len(p.Isr))
		for _, b := range p.Isr {
			isr = append(isr, strconv.Itoa(b.ID))
		}
		w.WriteLine(
			strconv.Itoa(p.ID),
			strconv.Itoa(p.Leader.ID),
			strings.Join(isr, ","),
			strconv.FormatInt(first[p.ID], 10),
			strconv.FormatInt(last[p.ID], 10),
			recordTime(c, topic, p.ID, first[p.ID]),
			recordTime(c, topic, p.ID, last[p.ID]-1),
		)
	}
	cfg, err := c.TopicConfig(ctx, topic)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(os.Stdout, "%-28s %s\n", k, cfg[k])
	}
	return nil
}

// recordTime reads the timestamp of one record at an absolute offset
// ("-" when the partition is empty there).
func recordTime(c *kq.Client, topic string, partition int, offset int64) string {
	conn, err := c.Conn(context.Background(), topic, partition)
	if err != nil {
		return "-"
	}
	defer conn.Close()
	last, err := conn.ReadLastOffset()
	if err != nil || offset < 0 || offset >= last {
		return "-"
	}
	if _, err := conn.Seek(offset, kafka.SeekAbsolute); err != nil {
		return "-"
	}
	m, err := conn.ReadMessage(1 << 20)
	if err != nil {
		return "-"
	}
	return m.Time.Local().Format("2006-01-02 15:04:05")
}

type kvFlags []string

func (k *kvFlags) String() string     { return strings.Join(*k, ",") }
func (k *kvFlags) Set(v string) error { *k = append(*k, v); return nil }

// buildBody resolves the send body: --payload k=v pairs win (JSON object),
// otherwise --body verbatim.
func buildBody(body string, payload []string) ([]byte, error) {
	if len(payload) > 0 {
		obj := map[string]string{}
		for _, kv := range payload {
			eq := strings.IndexByte(kv, '=')
			if eq <= 0 {
				return nil, fmt.Errorf("bad --payload %q (want k=v)", kv)
			}
			obj[kv[:eq]] = kv[eq+1:]
		}
		return json.Marshal(obj)
	}
	if body == "" {
		return nil, errors.New("give --body or at least one --payload k=v")
	}
	return []byte(body), nil
}

func cmdSend(ctx context.Context, c *kq.Client, args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	body := fs.String("body", "", "raw record value")
	var payload kvFlags
	fs.Var(&payload, "payload", "k=v pair for a JSON payload (repeatable)")
	key := fs.String("key", "", "record key")
	priority := fs.Int("priority", -1, "priority as a record header")
	topic, rest, ok := splitPos(args)
	fs.Parse(rest)
	if !ok {
		return errors.New("usage: send <topic> (--body S | --payload k=v ...) [--key K] [--priority N]")
	}
	value, err := buildBody(*body, payload)
	if err != nil {
		return err
	}
	var headers []kafka.Header
	if *priority >= 0 {
		headers = append(headers, kafka.Header{Key: "priority", Value: []byte(strconv.Itoa(*priority))})
	}
	partition, offset, err := c.Send(ctx, topic, []byte(*key), value, headers)
	if err != nil {
		return err
	}
	fmt.Printf("acked %s partition %d offset %d\n", topic, partition, offset)
	return nil
}

func cmdPurge(ctx context.Context, c *kq.Client, args []string) error {
	fs := flag.NewFlagSet("purge", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: purge <topic>")
	}
	start, err := c.Purge(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("purged %s: log start is now %v\n", fs.Arg(0), start)
	return nil
}

func cmdDrop(ctx context.Context, c *kq.Client, args []string) error {
	fs := flag.NewFlagSet("drop", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: drop <topic>")
	}
	if err := c.DeleteTopic(ctx, fs.Arg(0)); err != nil {
		return err
	}
	fmt.Printf("dropped %s\n", fs.Arg(0))
	return nil
}

// cmdParity reports the shadow parity per source queue (plan §5): the AMQP
// side (broker publish/deliver counters since kshadow's baselines), the
// shadow side (an independent scan of the source=amqp Kafka records) and
// kshadow's own written/mismatch accounting. Exit code: 0 = parity holds
// (and lag within --max-lag), 1 = a mismatch is present, 2 = lag over the
// limit on an active queue, 3 = kshadow's state is unreachable (it is down).
func cmdParity(ctx context.Context, c *kq.Client, args []string) int {
	fs := flag.NewFlagSet("parity", flag.ExitOnError)
	maxLag := fs.Duration("max-lag", 5*time.Minute, "fail when a queue's shadow lag exceeds this")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	statePath := fs.String("state", envOr("KSHADOW_STATE", "/home/agent/.local/share/jarvis/kshadow/kshadow-state.json"), "kshadow's state file")
	fs.Parse(args)

	// LoadState yields a fresh state for a missing file (kshadow's own
	// first run needs that); for the gate a missing file means kshadow
	// has not mirrored a single message yet, which is "down" (exit 3).
	if _, err := os.Stat(*statePath); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "jarvis-kq: kshadow state %s not found — has kshadow mirrored anything yet?\n", *statePath)
			return 3
		}
		fmt.Fprintf(os.Stderr, "jarvis-kq: kshadow state unreadable (%v) — is kshadow running?\n", err)
		return 3
	}
	state, err := kshadow.LoadState(*statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-kq: kshadow state unreadable (%v) — is kshadow running?\n", err)
		return 3
	}
	admin, err := kshadow.NewAdmin(envOr("KSHADOW_RABBITMQ_MGMT", envOr("RABBITMQ_URL", "amqp://jarvis:jarvis-local@localhost:5672")))
	if err != nil {
		fmt.Fprintln(os.Stderr, "jarvis-kq:", err)
		return 3
	}
	rep, err := (&kshadow.Parity{State: state, Admin: admin, Kafka: c}).Report(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "jarvis-kq:", err)
		return 3
	}

	if *asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
	} else {
		printParity(rep)
	}

	// The gate: zero mismatch, and lag within the limit on any queue that
	// has traffic (an idle queue's lag is meaningless).
	for _, r := range rep.Rows {
		if r.Mismatch > 0 || r.Dropped > 0 {
			return 1
		}
	}
	for _, r := range rep.Rows {
		if r.LastAt.IsZero() {
			continue
		}
		if d, err := time.ParseDuration(r.Lag); err == nil && d > *maxLag {
			return 2
		}
	}
	return 0
}

func printParity(rep *kshadow.Report) {
	fmt.Printf("kshadow %s (started %s, at %s)\n", rep.Kshadow, rep.StartedAt.Format(time.RFC3339), rep.At.Format(time.RFC3339))
	if rep.Kshadow == "down" {
		return
	}
	w := newTab()
	w.WriteLine("QUEUE", "EXCHANGE", "PUBLISHED", "MIRRORED", "WRITTEN", "CONSUMED", "READY", "UNACKED", "DROPPED", "MISMATCH", "LAG")
	for _, r := range rep.Rows {
		w.WriteLine(r.Queue, r.Exchange,
			fmtInt(r.Published), fmtInt(r.Mirrored), fmtInt(r.Written), fmtInt(r.Consumed),
			fmtInt(r.Ready), fmtInt(r.Unacked), fmtInt(r.Dropped), fmtInt(r.Mismatch), r.Lag)
	}
	_ = w.Flush()
}

// fmtInt renders a count; -1 (the unknown marker from parity) as "-".
func fmtInt(n int64) string {
	if n < 0 {
		return "-"
	}
	return strconv.FormatInt(n, 10)
}
