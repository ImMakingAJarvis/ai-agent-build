// jarvis-mq is the RabbitMQ side of event-driven work (docs/event-driven.md).
// It publishes and consumes on any queue — normally a per-flow result queue
// (flow.<name>) — so one model can hand work to another across rounds:
// publish the job, finish the round, let the other model do it, then come
// back and consume the result it left in the same queue.
//
// Usage:
//
//	jarvis-mq publish flow.avatar --kind agent --title "make avatar" \
//	    --instructions "generate the image, save it, publish the result to \
//	    flow.avatar, then add a local agent job that consumes flow.avatar" \
//	    --context "discord avatar for the identity DM, 512x512" \
//	    --payload size=512
//	jarvis-mq peek flow.avatar
//	jarvis-mq consume flow.avatar            # pops up to 1 message
//	jarvis-mq consume flow.avatar -n 5 --json
//	jarvis-mq depth flow.avatar
//	jarvis-mq queues
//	jarvis-mq purge flow.avatar              # drop the messages, keep the queue
//	jarvis-mq delete flow.avatar             # drop the queue too
//	jarvis-mq event job.photo.started --source worker
//	jarvis-mq tail events                    # follow all lifecycle events
//	jarvis-mq tail events 'job.#' -n 5       # stop after five job.* events
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("jarvis-mq: ")
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "publish":
		err = runPublish(os.Args[2:])
	case "consume":
		err = runConsume(os.Args[2:])
	case "peek":
		err = runPeek(os.Args[2:])
	case "depth":
		err = runDepth(os.Args[2:])
	case "queues":
		err = runQueues(os.Args[2:])
	case "purge":
		err = runPurge(os.Args[2:])
	case "delete":
		err = runDelete(os.Args[2:])
	case "event":
		err = runEvent(os.Args[2:])
	case "tail":
		err = runTail(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `jarvis-mq — RabbitMQ publish/consume for event-driven work (docs/event-driven.md)

  publish <queue> [flags]   publish one message (flow.<name> queues are created on demand)
  consume <queue> [-n N]    pop up to N messages (default 1), acking them
  peek <queue> [-n N]       show up to N messages without removing them
  depth <queue>             number of ready messages
  queues                    list all queues with their depths
  purge <queue>             remove all messages, keep the queue
  delete <queue>            delete the queue
  event <routing-key> [flags]  publish one lifecycle event to the events exchange
  tail <exchange> [pattern]   follow lifecycle events (pattern default "#")

event flags:
  --source S                source tag (default jarvis-mq)
  --job-id J                job id (defaults to the agent.<job-id> subject)
  --data k=v                event data key (repeatable)
tail flags:
  -n N                      stop after N events (0 = follow until Ctrl-C)

publish flags:
  --kind agent              agent job (ingested into the job store by the worker)
  --type T                  message type (default "event"; agent jobs usually "background")
  --id ID                   message id (default mq-<unix>)
  --title T                 short label
  --instructions I          full job text (agent jobs)
  --prompt P                prompt (legacy text jobs)
  --context C               free text left for the consumer's next round
  --for-job J               id of the job this message is the result of
  --payload k=v             extra payload key (repeatable)
  --driver D                agent driver (local, igpu, sonnet, ...)
  --class C                 agent class (fix, build, background)
  --effort E                reasoning effort (off, low, medium, high, xhigh, max)
  --priority N              AMQP priority (ignored on non-priority queues)
  --source S                source tag (default jarvis-mq)
`)
	os.Exit(2)
}

// payloadFlag collects repeatable --payload k=v pairs.
type payloadFlag map[string]string

func (p *payloadFlag) String() string { return "k=v" }
func (p *payloadFlag) Set(v string) error {
	i := strings.IndexByte(v, '=')
	if i <= 0 {
		return fmt.Errorf("--payload needs k=v, got %q", v)
	}
	(*p)[v[:i]] = v[i+1:]
	return nil
}

// defaultID builds a message id: the unix time (sortable) plus a short
// random suffix, so two publishes in the same second never collide (the
// worker uses the id as the agent-job id when it ingests kind=agent).
func defaultID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "mq-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return "mq-" + strconv.FormatInt(time.Now().Unix(), 10) + "-" + hex.EncodeToString(b)
}

func newConn() (*queue.Conn, error) {
	c, err := queue.NewConn()
	if err != nil {
		return nil, err
	}
	return c, nil
}

// takeQueue returns the queue — always the FIRST argument — and parses
// the rest into fs (Go's flag package stops at the first non-flag, so the
// queue has to come before the flags, like git's subcommand).
func takeQueue(fs *flag.FlagSet, args []string) (string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", fmt.Errorf("the queue is the first argument: jarvis-mq %s <queue> [flags]", fs.Name())
	}
	if err := fs.Parse(args[1:]); err != nil {
		return "", err
	}
	if fs.NArg() != 0 {
		return "", fmt.Errorf("unexpected extra argument %q", fs.Arg(0))
	}
	return args[0], nil
}

func runPublish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	kind := fs.String("kind", "", `"" plain message, "agent" agent job`)
	jobType := fs.String("type", "event", "message type")
	id := fs.String("id", defaultID(), "message id (unique per publish)")
	title := fs.String("title", "", "short label")
	instructions := fs.String("instructions", "", "full job text (agent jobs)")
	prompt := fs.String("prompt", "", "prompt (legacy text jobs)")
	ctxText := fs.String("context", "", "free text left for the consumer's next round")
	forJob := fs.String("for-job", "", "id of the job this message is the result of")
	payload := &payloadFlag{}
	fs.Var(payload, "payload", "extra payload key (repeatable)")
	driver := fs.String("driver", "", "agent driver")
	class := fs.String("class", "", "agent class (fix, build, background)")
	effort := fs.String("effort", "", "reasoning effort")
	priority := fs.Int("priority", 1, "AMQP priority")
	source := fs.String("source", "jarvis-mq", "source tag")
	q, err := takeQueue(fs, args)
	if err != nil {
		return err
	}

	m := queue.Message{
		ID:           *id,
		Type:         *jobType,
		Priority:     *priority,
		CreatedAt:    time.Now().UTC(),
		Source:       *source,
		Payload:      map[string]string(*payload),
		Kind:         *kind,
		Title:        *title,
		Instructions: *instructions,
		Driver:       *driver,
		Class:        *class,
	}
	if *prompt != "" {
		m.Payload["prompt"] = *prompt
	}
	if *ctxText != "" {
		m.Payload["context"] = *ctxText
	}
	if *forJob != "" {
		m.Payload["for_job"] = *forJob
	}
	if *effort != "" {
		m.ReasoningEffort = *effort
	}
	// A message must carry something its consumer will read: the job text
	// (--instructions), a legacy prompt, context left for a later round, or
	// --payload entries (a result message is often payload-only).
	if m.Instructions == "" && m.Payload["prompt"] == "" && m.Payload["context"] == "" && len(m.Payload) == 0 {
		return fmt.Errorf("give the message something: --prompt, --instructions, --context, or --payload")
	}

	conn, err := newConn()
	if err != nil {
		return err
	}
	defer conn.Close()

	// Flow queues are created on demand; known queues (voice, fix, ... and
	// jobs.*) already exist and are published to with the standard routing
	// key. A message to an unknown non-flow queue would be dropped by the
	// topic exchange (unbound routing key), so refuse it.
	if queue.IsFlowQueue(q) {
		if err := conn.PublishFlow(context.Background(), q, m); err != nil {
			return err
		}
		fmt.Printf("published %s → %s (flow queue created on demand)\n", m.ID, q)
		return nil
	}
	for _, wq := range workerOwnedQueues {
		if q == wq {
			if err := conn.Publish(m, q+".default"); err != nil {
				return err
			}
			fmt.Printf("published %s → %s\n", m.ID, q)
			return nil
		}
	}
	return fmt.Errorf("queue %q is not a flow queue (flow.*) or a known queue; the exchange would drop it", q)
}

func runConsume(args []string) error {
	fs := flag.NewFlagSet("consume", flag.ExitOnError)
	n := fs.Int("n", 1, "max messages to pop")
	asJSON := fs.Bool("json", false, "raw JSON, one message per line")
	q, err := takeQueue(fs, args)
	if err != nil {
		return err
	}
	conn, err := newConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	msgs, err := conn.PopFlow(q, *n)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		fmt.Printf("%s: empty\n", q)
		return nil
	}
	return printMessages(msgs, *asJSON)
}

func runPeek(args []string) error {
	fs := flag.NewFlagSet("peek", flag.ExitOnError)
	n := fs.Int("n", 1, "max messages to show")
	asJSON := fs.Bool("json", false, "raw JSON, one message per line")
	q, err := takeQueue(fs, args)
	if err != nil {
		return err
	}
	conn, err := newConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	msgs, err := conn.PeekFlow(q, *n)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		fmt.Printf("%s: empty\n", q)
		return nil
	}
	return printMessages(msgs, *asJSON)
}

func printMessages(msgs []queue.Message, asJSON bool) error {
	for _, m := range msgs {
		if asJSON {
			b, err := json.Marshal(m)
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			continue
		}
		fmt.Printf("--- %s (type=%s kind=%s priority=%d source=%s %s)\n",
			m.ID, m.Type, m.Kind, m.Priority, m.Source, m.CreatedAt.UTC().Format(time.RFC3339))
		if m.Title != "" {
			fmt.Printf("title:        %s\n", m.Title)
		}
		if m.Instructions != "" {
			fmt.Printf("instructions: %s\n", m.Instructions)
		}
		if m.Driver != "" || m.Class != "" || m.ReasoningEffort != "" {
			fmt.Printf("driver=%s class=%s effort=%s\n", m.Driver, m.Class, m.ReasoningEffort)
		}
		for _, k := range sortedKeys(m.Payload) {
			fmt.Printf("payload[%s]: %s\n", k, m.Payload[k])
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func runDepth(args []string) error {
	fs := flag.NewFlagSet("depth", flag.ExitOnError)
	q, err := takeQueue(fs, args)
	if err != nil {
		return err
	}
	conn, err := newConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	d, err := conn.QueueDepth(q)
	if err != nil {
		return err
	}
	fmt.Println(d)
	return nil
}

// mgmtAPIURL derives the management HTTP URL from RABBITMQ_URL (the
// broker's standard companion: amqp port 5672 → management 15672, same
// user and password).
func mgmtAPIURL() (string, error) {
	u := os.Getenv("RABBITMQ_URL")
	if u == "" {
		u = "amqp://jarvis:jarvis-local@localhost:5672"
	}
	p, err := url.Parse(u)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", u, err)
	}
	host := p.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = net.JoinHostPort(h, "15672")
	}
	var user string
	if p.User != nil {
		pw, _ := p.User.Password()
		user = url.UserPassword(p.User.Username(), pw).String() + "@"
	}
	return "http://" + user + host + "/api/queues", nil
}

func runQueues(args []string) error {
	fs := flag.NewFlagSet("queues", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: jarvis-mq queues")
	}
	apiURL, err := mgmtAPIURL()
	if err != nil {
		return err
	}
	resp, err := http.Get(apiURL)
	if err != nil {
		return fmt.Errorf("management API (port 15672): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("management API: HTTP %d", resp.StatusCode)
	}
	var qs []struct {
		Name     string `json:"name"`
		Messages int    `json:"messages"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&qs); err != nil {
		return fmt.Errorf("decode queues: %w", err)
	}
	sort.Slice(qs, func(i, j int) bool { return qs[i].Name < qs[j].Name })
	for _, q := range qs {
		fmt.Printf("%-20s %d\n", q.Name, q.Messages)
	}
	return nil
}

func runPurge(args []string) error {
	fs := flag.NewFlagSet("purge", flag.ExitOnError)
	q, err := takeQueue(fs, args)
	if err != nil {
		return err
	}
	conn, err := newConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	n, err := conn.PurgeFlow(q)
	if err != nil {
		return err
	}
	fmt.Printf("purged %d message(s) from %s\n", n, q)
	return nil
}

// workerOwnedQueues are the queues the worker (or a planned worker)
// declares and consumes. Deleting one out from under a running consumer
// loses a delivery, so delete refuses them. "image" is not yet a constant
// in the queue package (it lands with the photo pipeline); it is listed
// here by name so a stray "jarvis-mq delete image" cannot take the
// photo pipeline's queue down.
var workerOwnedQueues = []string{
	queue.VoiceQueue, queue.FixQueue, queue.CodingQueue, queue.BackgroundQueue,
	queue.JobsGPUQueue, queue.JobsCPUQueue, queue.JobsIGPUQueue, "image",
}

func runDelete(args []string) error {
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	q, err := takeQueue(fs, args)
	if err != nil {
		return err
	}
	for _, wq := range workerOwnedQueues {
		if q == wq {
			return fmt.Errorf("refusing to delete worker-owned queue %q", q)
		}
	}
	conn, err := newConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.DeleteFlow(q); err != nil {
		return err
	}
	fmt.Printf("deleted %s\n", q)
	return nil
}

// runEvent publishes one lifecycle event to the events exchange
// (plan §3.4). The routing key is the first argument, so
// "jarvis-mq event agent.plan-x.resumed --source agent-run" reads like
// the key family it publishes.
func runEvent(args []string) error {
	fs := flag.NewFlagSet("event", flag.ExitOnError)
	source := fs.String("source", "jarvis-mq", "source tag")
	jobID := fs.String("job-id", "", "job id (defaults to the agent.<job-id> subject)")
	data := &payloadFlag{}
	fs.Var(data, "data", "event data key (repeatable)")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("the routing key is the first argument: jarvis-mq event <routing-key> [flags]")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected extra argument %q", fs.Arg(0))
	}
	kind, subject, state, err := queue.ParseEventRoutingKey(args[0])
	if err != nil {
		return err
	}
	switch kind {
	case queue.JobKind, queue.AgentKind, queue.WindowKind, queue.LeaseKind:
	default:
		return fmt.Errorf("unknown event kind %q (want job, agent, window or lease)", kind)
	}
	e := queue.Event{Kind: kind, Subject: subject, State: state, Source: *source}
	if *jobID != "" {
		e.JobID = *jobID
	} else if kind == queue.AgentKind {
		e.JobID = subject
	}
	if len(*data) > 0 {
		e.Data = make(map[string]any, len(*data))
		for k, v := range *data {
			e.Data[k] = v
		}
	}
	conn, err := newConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	p, err := queue.NewEventPublisher(conn, *source)
	if err != nil {
		return err
	}
	defer p.Close()
	if err := p.Publish(context.Background(), e); err != nil {
		return err
	}
	fmt.Printf("event published: %s (source=%s)\n", e.RoutingKey(), e.Source)
	return nil
}

// runTail follows lifecycle events: a temporary auto-delete queue bound
// to the exchange with the pattern, one line per event. With -n N it
// stops after N events (the Phase 1 gate's "events visible" check); with
// -n 0 it follows until Ctrl-C.
func runTail(args []string) error {
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	n := fs.Int("n", 0, "stop after N events (0 = follow until Ctrl-C)")
	// Two positionals (exchange, pattern) before the flags, same
	// convention as takeQueue — split at the first flag.
	pos, flgs := args, []string(nil)
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			pos, flgs = args[:i], args[i:]
			break
		}
	}
	if err := fs.Parse(flgs); err != nil {
		return err
	}
	if fs.NArg() != 0 || len(pos) < 1 || len(pos) > 2 {
		return fmt.Errorf("usage: jarvis-mq tail <exchange> [pattern] [-n N]")
	}
	exchange := pos[0]
	pattern := "#"
	if len(pos) == 2 {
		pattern = pos[1]
	}
	conn, err := newConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.NewChannel()
	if err != nil {
		return err
	}
	defer ch.Close()
	// Declare the exchange too: it is durable (definitions.json), but a
	// tail run on a broker that restarted before any publisher booted
	// would otherwise fail the bind.
	if err := ch.ExchangeDeclare(exchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %s: %w", exchange, err)
	}
	b := make([]byte, 3)
	rand.Read(b)
	name := "jarvis-mq.tail." + strconv.FormatInt(time.Now().Unix(), 10) + "-" + hex.EncodeToString(b)
	if _, err := ch.QueueDeclare(name, false, true, false, false, nil); err != nil {
		return err
	}
	if err := ch.QueueBind(name, pattern, exchange, false, nil); err != nil {
		return fmt.Errorf("bind %s: %w", name, err)
	}
	dels, err := ch.Consume(name, "", false, true, false, false, nil)
	if err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	count := 0
	for {
		select {
		case <-sig:
			fmt.Fprintf(os.Stderr, "%d event(s) seen\n", count)
			return nil
		case d, ok := <-dels:
			if !ok {
				return nil
			}
			printEvent(d)
			count++
			if *n > 0 && count >= *n {
				return nil
			}
		}
	}
}

func printEvent(d amqp.Delivery) {
	e, err := queue.DecodeEvent(d.Body)
	if err != nil {
		fmt.Printf("=== %s (not a queue.Event: %v)\n%s\n", d.RoutingKey, err, string(d.Body))
		return
	}
	fmt.Printf("=== %s (source=%s", d.RoutingKey, e.Source)
	if e.JobID != "" {
		fmt.Printf(" job=%s", e.JobID)
	}
	fmt.Printf(" at=%s)\n%s\n", e.At.UTC().Format(time.RFC3339), string(d.Body))
}
