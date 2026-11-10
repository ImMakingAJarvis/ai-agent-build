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
//
// Capability loop (plan §3.5, task EVB-15):
//
//	jarvis-mq listen flow.plan-x --wake plan-x --timeout 2h
//	jarvis-mq submit image.generate --reply-to flow.plan-x --corr plan-x-img-1 \
//	    --input prompt="a lighthouse at dusk" --out-dir ~/.local/share/jarvis/artifacts/plan-x/
//	jarvis-mq listeners
//	jarvis-mq results flow.plan-x          # consume + pretty-print Results (acks)
//	jarvis-mq unlisten flow.plan-x
//	jarvis-mq caps                         # catalog: where/cost/depth/runtime per capability
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/catalog"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/switchboard"
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
	case "listen":
		err = runListen(os.Args[2:])
	case "unlisten":
		err = runUnlisten(os.Args[2:])
	case "listeners":
		err = runListeners(os.Args[2:])
	case "submit":
		err = runSubmit(os.Args[2:])
	case "results":
		err = runResults(os.Args[2:])
	case "caps":
		err = runCaps(os.Args[2:])
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

  publish <queue> [flags]   publish one message (flow.<name> and cap.<cap> queues are created on demand)
  consume <queue> [-n N]    pop up to N messages (default 1), acking them
  peek <queue> [-n N]       show up to N messages without removing them
  depth <queue>             number of ready messages
  queues                    list all queues with their depths
  purge <queue>             remove all messages, keep the queue
  delete <queue>            delete the queue
  event <routing-key> [flags]  publish one lifecycle event to the events exchange
  tail <exchange> [pattern]   follow lifecycle events (pattern default "#")
  listen <queue> [flags]      attach a switchboard listener (wake a job, forward, or notify)
  unlisten <queue> [flags]    remove the queue's listeners (--corr to narrow)
  listeners                   list all listeners (queue, action, target, timeout)
  submit <capability> [flags] publish an envelope-v2 request to cap.<capability>
  results <queue> [-n N]      consume and pretty-print Result messages (acks them)
  caps [--file PATH]          the capability catalog, one line per capability:
                              where it runs, the cost, current queue depth,
                              typical runtime (readable by the model as-is)

listen flags:
  --wake JOB                wake agentjob JOB when a message lands (default action)
  --forward Q               instead: republish the message to Q and ack it
  --notify TEXT             instead: send TEXT as a Discord notification and ack
  --timeout D               give up after D (default 0 = never; e.g. 2h)
  --corr C                  only a message with this correlation id triggers
unlisten flags:
  --corr C                  only remove listeners filtered on this correlation id
submit flags:
  --reply-to Q              queue (or flow) the Result is published to (flow.<job-id>)
  --corr C                  correlation id tying the request to its result
  --input k=v               capability argument (repeatable; JSON-typed: 512, true, [..])
  --input-ref P             input file or dir the worker reads, by path (repeatable)
  --out-dir D               where the worker writes its artifacts
  --location L              pin to a device: gpu | igpu | cpu | remote:<host>
  --vram-mb N               VRAM hint for the resource broker
  --ram-mb N                RAM hint for the resource broker
  --deadline T              give up after T (RFC3339 or a duration like 2h from now)
  --wake-job J              agentjob id a worker may wake directly if the switchboard is down
  --parent-job J            id of the job that submitted this one
results flags:
  -n N                      max messages to pop (default 100: drain what has arrived)
  --json                    raw JSON bodies, one per line

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
  --capability C            capability requested (image.generate, ...) — envelope v2
  --correlation C           correlation id tying the request to its result — v2
  --reply-to Q              queue (or flow) the result is published to — v2
  --wake-job J              agentjob id to wake when the result lands — v2
  --parent-job J            id of the job that submitted this one — v2
  --location L              pin the work to a device (gpu | igpu | cpu | remote:<host>) — v2
  --out-dir D               where the worker writes its artifacts — v2
  --input-ref P             input file or dir the worker reads, by path — v2, repeatable
`)
	os.Exit(2)
}

// stringListFlag collects repeatable string flags (e.g. --input-ref).
type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, ", ") }
func (s *stringListFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
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
	// Envelope v2 (plan §3.3): capability request fields. Setting any of
	// them marks the message v2 (v=2); an unset field stays out of the JSON
	// and the AMQP properties (omitempty).
	capability := fs.String("capability", "", "capability requested (image.generate, ...) — makes the message v2")
	correlation := fs.String("correlation", "", "correlation id tying the request to its result — v2")
	replyTo := fs.String("reply-to", "", "queue (or flow) the result is published to — v2")
	wakeJob := fs.String("wake-job", "", "agentjob id to wake when the result lands — v2")
	parentJob := fs.String("parent-job", "", "id of the job that submitted this one — v2")
	location := fs.String("location", "", "pin the work to a device: gpu | igpu | cpu | remote:<host> — v2")
	outDir := fs.String("out-dir", "", "where the worker writes its artifacts — v2")
	inputRefs := &stringListFlag{}
	fs.Var(inputRefs, "input-ref", "input file or dir the worker reads, by path — v2, repeatable")
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
	// Envelope v2: set the request fields and mark the envelope v2. Any v2
	// field makes the message a capability request (plan §3.3).
	if *capability != "" {
		m.Capability = *capability
	}
	if *correlation != "" {
		m.CorrelationID = *correlation
	}
	if *replyTo != "" {
		m.ReplyTo = *replyTo
	}
	if *wakeJob != "" {
		m.WakeJob = *wakeJob
	}
	if *parentJob != "" {
		m.ParentJob = *parentJob
	}
	if *location != "" {
		m.Location = *location
	}
	if *outDir != "" {
		m.OutDir = *outDir
	}
	if len(*inputRefs) > 0 {
		m.InputRefs = *inputRefs
	}
	if m.Capability != "" || m.CorrelationID != "" || m.ReplyTo != "" || m.WakeJob != "" ||
		m.ParentJob != "" || m.Location != "" || m.OutDir != "" || len(m.InputRefs) > 0 {
		m.SchemaVersion = 2
	}
	// A message must carry something its consumer will read: the job text
	// (--instructions), a legacy prompt, context left for a later round,
	// --payload entries (a result message is often payload-only), or a v2
	// capability field (the request's arguments ride on Input/InputRefs).
	if m.Instructions == "" && m.Payload["prompt"] == "" && m.Payload["context"] == "" &&
		len(m.Payload) == 0 && m.SchemaVersion == 0 {
		return fmt.Errorf("give the message something: --prompt, --instructions, --context, --payload, or a --capability field")
	}

	conn, err := newConn()
	if err != nil {
		return err
	}
	defer conn.Close()

	// Flow and capability queues are created on demand (both carry the
	// dead-letter args, EVB-10); known queues (voice, fix, ... and jobs.*)
	// already exist and are published to with the standard routing key. A
	// message to an unknown non-flow queue would be dropped by the topic
	// exchange (unbound routing key), so refuse it.
	if queue.IsFlowQueue(q) {
		if err := conn.PublishFlow(context.Background(), q, m); err != nil {
			return err
		}
		fmt.Printf("published %s → %s (flow queue created on demand)\n", m.ID, q)
		return nil
	}
	if queue.IsCapQueue(q) {
		if err := conn.EnsureCapQueue(q); err != nil {
			return err
		}
		if err := conn.Publish(m, q+".default"); err != nil {
			return err
		}
		fmt.Printf("published %s → %s (capability queue created on demand)\n", m.ID, q)
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
	return fmt.Errorf("queue %q is not a flow queue (flow.*), a capability queue (cap.*), or a known queue; the exchange would drop it", q)
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
	if queue.IsCapQueue(q) {
		// Capability queues hold in-flight requests for a capability worker
		// (capworker SDK, EVB-19) — same reason worker-owned queues are
		// protected: deleting one loses a delivery.
		return fmt.Errorf("refusing to delete capability queue %q", q)
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

// --- the capability loop (plan §3.5, task EVB-15) ------------------------
//
// listen / unlisten / listeners drive the switchboard's listener store
// (internal/switchboard, EVB-14): JSON files under
// ~/.local/share/jarvis/listeners/ — the interface the switchboard chose
// (it exposes /metrics only, no control HTTP). submit publishes an
// envelope-v2 request to cap.<capability>; consumer priorities do the
// placement, so there is no router here. results consumes the Result
// messages a worker leaves on the reply queue for the resuming round.

// listenersDir is where the switchboard keeps its listeners: $LISTENERS_DIR
// or <agentjob data dir>/listeners — the same defaults cmd/switchboard
// boots with, so the CLI and the service always agree.
func listenersDir() string {
	if d := os.Getenv("LISTENERS_DIR"); d != "" {
		return d
	}
	return filepath.Join(agentjob.DataDir(), "listeners")
}

// listenerID builds a unique listener id (it is a file name in the store):
// the watched queue, the corr filter when set, and a time+random suffix so
// two listens on the same queue never collide.
func listenerID(queue, corr string) string {
	id := "lsn-" + queue
	if corr != "" {
		id += "-" + corr
	}
	b := make([]byte, 3)
	rand.Read(b)
	return id + "-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + hex.EncodeToString(b)
}

// attachListener validates, builds, and stores one listener. action is one
// of switchboard.ActionWake/ActionForward/ActionNotify; JobID, TargetQueue
// and Text follow the action. The store's Create runs the same validation
// the service runs (wake needs a job, forward a target).
func attachListener(st *switchboard.Store, queue, action, jobID, targetQueue, text, corr string, timeout time.Duration) (switchboard.Listener, error) {
	l := switchboard.Listener{
		ID:          listenerID(queue, corr),
		Queue:       queue,
		Action:      action,
		JobID:       jobID,
		TargetQueue: targetQueue,
		Text:        text,
		CorrFilter:  corr,
		Timeout:     timeout,
	}
	if err := st.Create(l); err != nil {
		return switchboard.Listener{}, err
	}
	return l, nil
}

// detachListeners removes the listeners on queue (narrowed to corr when
// given) and returns how many were removed. Removing none is not an error:
// the switchboard treats a missing listener as already gone, and so does
// unlisten.
func detachListeners(st *switchboard.Store, queue, corr string) (int, error) {
	all, err := st.List()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range all {
		if l.Queue != queue || (corr != "" && l.CorrFilter != corr) {
			continue
		}
		if err := st.Remove(l.ID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// runListen attaches a switchboard listener on a queue (plan §3.2 step 2):
// when a message lands, wake the job (the default), forward it to another
// queue, or send a Discord notification. Flow and capability queues are
// declared on demand so the switchboard's first peek finds them; a listener
// may also watch a queue a worker declares (nothing to declare then).
func runListen(args []string) error {
	fs := flag.NewFlagSet("listen", flag.ExitOnError)
	wake := fs.String("wake", "", "wake this agentjob when a message lands (default action)")
	forward := fs.String("forward", "", "instead: republish the message to this queue and ack it")
	notify := fs.String("notify", "", "instead: send this Discord notification and ack the message")
	timeout := fs.Duration("timeout", 0, "give up after this long (0 = never)")
	corr := fs.String("corr", "", "only a message with this correlation id triggers the listener")
	q, err := takeQueue(fs, args)
	if err != nil {
		return err
	}
	// Exactly one action: wake, forward, or notify.
	actions := 0
	for _, s := range []string{*wake, *forward, *notify} {
		if s != "" {
			actions++
		}
	}
	if actions != 1 {
		return fmt.Errorf("listen: give exactly one of --wake JOB, --forward QUEUE, --notify TEXT")
	}
	action := switchboard.ActionWake
	switch {
	case *forward != "":
		action = switchboard.ActionForward
	case *notify != "":
		action = switchboard.ActionNotify
	}
	st, err := newListenerStore()
	if err != nil {
		return err
	}
	l, err := attachListener(st, q, action, *wake, *forward, *notify, *corr, *timeout)
	if err != nil {
		return err
	}
	conn, err := newConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	switch {
	case queue.IsFlowQueue(q):
		if err := conn.EnsureFlowQueue(q); err != nil {
			return err
		}
	case queue.IsCapQueue(q):
		if err := conn.EnsureCapQueue(q); err != nil {
			return err
		}
	}
	var target string
	switch action {
	case switchboard.ActionWake:
		target = "wake " + *wake
	case switchboard.ActionForward:
		target = "forward → " + *forward
	case switchboard.ActionNotify:
		target = "notify"
	}
	to := "never"
	if *timeout > 0 {
		to = timeout.String()
	}
	fmt.Printf("listening on %s: %s (corr=%s timeout=%s id=%s)\n", q, target, orDash(*corr), to, l.ID)
	return nil
}

// newListenerStore opens the switchboard listener store (the JSON file
// store under listenersDir).
func newListenerStore() (*switchboard.Store, error) {
	return switchboard.OpenStore(listenersDir())
}

// runUnlisten removes the listeners on a queue (plan §3.2 step 12): --corr
// narrows to the listeners filtered on that correlation id, without it the
// whole queue goes quiet.
func runUnlisten(args []string) error {
	fs := flag.NewFlagSet("unlisten", flag.ExitOnError)
	corr := fs.String("corr", "", "only remove listeners filtered on this correlation id")
	q, err := takeQueue(fs, args)
	if err != nil {
		return err
	}
	st, err := newListenerStore()
	if err != nil {
		return err
	}
	n, err := detachListeners(st, q, *corr)
	if err != nil {
		return err
	}
	suffix := ""
	if *corr != "" {
		suffix = fmt.Sprintf(" (corr=%s)", *corr)
	}
	if n == 0 {
		fmt.Printf("no listener on %s%s\n", q, suffix)
		return nil
	}
	fmt.Printf("removed %d listener(s) on %s%s\n", n, q, suffix)
	return nil
}

// runListeners lists every stored listener with what it does — the
// "who is waiting on what" view of the event loop.
func runListeners(args []string) error {
	fs := flag.NewFlagSet("listeners", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: jarvis-mq listeners")
	}
	st, err := newListenerStore()
	if err != nil {
		return err
	}
	all, err := st.List()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Println("no listeners")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "QUEUE\tACTION\tTARGET\tCORR\tTIMEOUT\tCREATED\tID")
	for _, l := range all {
		target := l.JobID
		switch l.Action {
		case switchboard.ActionForward:
			target = "→ " + l.TargetQueue
		case switchboard.ActionNotify:
			target = l.Text
		}
		to := "-"
		if l.Timeout > 0 {
			to = l.Timeout.String()
		}
		fmt.Fprintln(w, strings.Join([]string{
			l.Queue, l.Action, target, orDash(l.CorrFilter), to,
			l.CreatedAt.UTC().Format("2006-01-02 15:04:05"), l.ID,
		}, "\t"))
	}
	return w.Flush()
}

// inputFlag collects repeatable --input k=v pairs. The value is
// JSON-typed where it parses (512 → number, true → bool, [1,2] → list);
// anything that is not JSON stays a plain string — so prompt=a lighthouse
// and prompt="a lighthouse" both land as strings, and width=1024 as a number.
type inputFlag map[string]any

func (i *inputFlag) String() string { return "k=v" }
func (i *inputFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("--input needs k=v, got %q", v)
	}
	var typed any
	if err := json.Unmarshal([]byte(val), &typed); err != nil {
		typed = val // not JSON: a plain string
	}
	(*i)[k] = typed
	return nil
}

// parseDeadline parses --deadline: an absolute RFC3339 time, or a duration
// taken from now ("2h").
func parseDeadline(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return time.Now().UTC().Add(d), nil
	}
	return time.Time{}, fmt.Errorf("parse %q as RFC3339 or a duration from now (2h)", s)
}

// submitParams is everything `submit` takes from the command line, so the
// envelope build is testable without a broker.
type submitParams struct {
	capability string
	id         string
	replyTo    string
	corr       string
	input      map[string]any
	inputRefs  []string
	outDir     string
	location   string
	wakeJob    string
	parentJob  string
	vramMB     int
	ramMB      int
	deadline   *time.Time
}

// buildSubmitMessage builds the envelope-v2 capability request (plan §3.3).
// Every v2 field is set from the params and the envelope is marked v2;
// CorrelationID and ReplyTo are also mirrored onto the AMQP properties at
// publish time by queue.newPublishing.
func buildSubmitMessage(p submitParams) (queue.Message, error) {
	if p.capability == "" {
		return queue.Message{}, fmt.Errorf("submit: a capability is required (e.g. image.generate)")
	}
	if p.replyTo == "" {
		return queue.Message{}, fmt.Errorf("submit: --reply-to is required (the queue the Result lands on, flow.<job-id>)")
	}
	if p.corr == "" {
		return queue.Message{}, fmt.Errorf("submit: --corr is required (ties the request to its Result; handlers are idempotent by it)")
	}
	m := queue.Message{
		ID:            p.id,
		Type:          "capability",
		CreatedAt:     time.Now().UTC(),
		Source:        "jarvis-mq",
		Payload:       map[string]string{},
		SchemaVersion: 2,
		Capability:    p.capability,
		ReplyTo:       p.replyTo,
		CorrelationID: p.corr,
		Input:         p.input,
		InputRefs:     p.inputRefs,
		OutDir:        p.outDir,
		Location:      p.location,
		WakeJob:       p.wakeJob,
		ParentJob:     p.parentJob,
		Deadline:      p.deadline,
	}
	if p.vramMB > 0 || p.ramMB > 0 {
		m.Resources = &queue.Resources{VRAMMB: p.vramMB, RAMMB: p.ramMB}
	}
	return m, nil
}

// validCapabilityName reports whether s is acceptable as a `submit`
// capability. The convention is a dotted path (image.generate,
// audio.transcribe); a single word is allowed too, because the cap.echo test
// worker (EVB-17) is single-word and the Phase-2 gate submits to it.
// Segments are lowercase alphanumeric. A message to a capability with no
// worker simply waits on its durable queue until the request's deadline, so a
// misspelling degrades to the designed timeout path rather than being dropped
// at submit time.
func validCapabilityName(s string) bool {
	if s == "" {
		return false
	}
	for _, seg := range strings.Split(s, ".") {
		if !isCapSegment(seg) {
			return false
		}
	}
	return true
}

func isCapSegment(seg string) bool {
	if seg == "" {
		return false
	}
	for _, r := range seg {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// runSubmit publishes an envelope-v2 request to cap.<capability> (plan
// §3.2 step 3). Consumer priorities do the placement — the message goes to
// the queue and the broker delivers it to the best free worker; there is no
// router here.
func runSubmit(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	replyTo := fs.String("reply-to", "", "queue the Result is published to (flow.<job-id>)")
	corr := fs.String("corr", "", "correlation id tying the request to its result")
	input := &inputFlag{}
	fs.Var(input, "input", "capability argument k=v (repeatable, JSON-typed)")
	inputRefs := &stringListFlag{}
	fs.Var(inputRefs, "input-ref", "input file or dir the worker reads, by path (repeatable)")
	outDir := fs.String("out-dir", "", "where the worker writes its artifacts")
	location := fs.String("location", "", "pin to a device: gpu | igpu | cpu | remote:<host>")
	vramMB := fs.Int("vram-mb", 0, "VRAM hint in MB for the resource broker")
	ramMB := fs.Int("ram-mb", 0, "RAM hint in MB for the resource broker")
	deadline := fs.String("deadline", "", "give up after this (RFC3339 or a duration from now)")
	wakeJob := fs.String("wake-job", "", "agentjob id a worker may wake directly if the switchboard is down")
	parentJob := fs.String("parent-job", "", "id of the job that submitted this one")
	id := fs.String("id", defaultID(), "message id (unique per publish)")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("the capability is the first argument: jarvis-mq submit <capability> [flags]")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected extra argument %q", fs.Arg(0))
	}
	capability := args[0]
	if !validCapabilityName(capability) {
		return fmt.Errorf("capability %q is not valid: a dotted path (image.generate) or a single word (echo), lowercase alphanumeric segments", capability)
	}
	var dl *time.Time
	if *deadline != "" {
		t, err := parseDeadline(*deadline)
		if err != nil {
			return err
		}
		dl = &t
	}
	m, err := buildSubmitMessage(submitParams{
		capability: capability, id: *id, replyTo: *replyTo, corr: *corr,
		input: *input, inputRefs: *inputRefs, outDir: *outDir,
		location: *location, wakeJob: *wakeJob, parentJob: *parentJob,
		vramMB: *vramMB, ramMB: *ramMB, deadline: dl,
	})
	if err != nil {
		return err
	}
	conn, err := newConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	qname := queue.CapQueueName(capability)
	if err := conn.EnsureCapQueue(qname); err != nil {
		return err
	}
	if err := conn.Publish(m, qname+".default"); err != nil {
		return err
	}
	fmt.Printf("published %s → %s (corr=%s reply-to=%s)\n", m.ID, qname, m.CorrelationID, m.ReplyTo)
	return nil
}

// runResults consumes the Result messages a worker left on a reply queue
// and pretty-prints them for the resuming round (plan §3.2 step 11) —
// acking them, since the round that reads them owns them. A message that
// is not a Result is printed in the plain message format rather than
// swallowed.
func runResults(args []string) error {
	fs := flag.NewFlagSet("results", flag.ExitOnError)
	n := fs.Int("n", 100, "max messages to pop (default 100: drain what has arrived)")
	asJSON := fs.Bool("json", false, "raw JSON bodies, one per line")
	q, err := takeQueue(fs, args)
	if err != nil {
		return err
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
	count := 0
	for count < *n {
		del, ok, err := ch.Get(q, true)
		if err != nil {
			return fmt.Errorf("results %s: get: %w", q, err)
		}
		if !ok {
			break // queue empty
		}
		count++
		if *asJSON {
			fmt.Println(string(del.Body))
			continue
		}
		res, rerr := queue.DecodeResult(del.Body)
		if rerr == nil && res.Status != "" {
			printResult(res)
			continue
		}
		m, merr := queue.DecodeMessage(del.Body)
		if merr != nil {
			fmt.Printf("=== %s (undecodable, acked)\n%s\n", q, string(del.Body))
			continue
		}
		if err := printMessages([]queue.Message{m}, false); err != nil {
			return err
		}
	}
	if count == 0 {
		fmt.Printf("%s: no results\n", q)
	}
	return nil
}

// printResult pretty-prints one Result (plan §3.3): the summary the brain
// reads first, then artifacts (references, never bytes), the small
// structured data, and the error when the status is not ok.
func printResult(r queue.Result) {
	fmt.Printf("=== result (corr=%s capability=%s status=%s)\n", r.CorrelationID, r.Capability, r.Status)
	if r.Worker != "" {
		fmt.Printf("    worker:   %s\n", r.Worker)
	}
	if r.Model != "" {
		fmt.Printf("    model:    %s\n", r.Model)
	}
	if !r.StartedAt.IsZero() {
		line := "    started:  " + r.StartedAt.UTC().Format(time.RFC3339)
		if !r.FinishedAt.IsZero() {
			line += fmt.Sprintf("  →  %s (%s)", r.FinishedAt.UTC().Format(time.RFC3339), r.FinishedAt.Sub(r.StartedAt).Round(time.Second))
		}
		fmt.Println(line)
	}
	if r.Summary != "" {
		fmt.Printf("    summary:  %s\n", r.Summary)
	}
	if len(r.Artifacts) > 0 {
		fmt.Println("    artifacts:")
		for _, a := range r.Artifacts {
			fmt.Printf("      %s\n", a)
		}
	}
	if len(r.Data) > 0 {
		fmt.Println("    data:")
		for _, k := range sortedKeysAny(r.Data) {
			fmt.Printf("      %s: %v\n", k, r.Data[k])
		}
	}
	if r.Error != "" {
		fmt.Printf("    error:    %s\n", r.Error)
	}
}

func sortedKeysAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// catalogDefaultPath is where the capability catalog lives on this
// machine: $JARVIS_CAPABILITIES, else the repo's config/capabilities.yaml.
func catalogDefaultPath() string {
	if p := os.Getenv("JARVIS_CAPABILITIES"); p != "" {
		return p
	}
	return "/home/agent/ai-agent-build/config/capabilities.yaml"
}

// capDepths connects to the broker and returns a depth function for cap
// queues (a missing queue is depth 0 — nothing queued). ok=false when
// the broker is unreachable; the caller then prints "-" depths, because
// the catalog is still useful when the broker is down. The connection is
// left open: the command prints and the process exits.
func capDepths() (func(string) (int, error), bool) {
	c, err := queue.NewConn()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-mq: broker unreachable (%v); printing depths as -\n", err)
		return nil, false
	}
	return func(name string) (int, error) {
		d, err := c.QueueDepth(queue.CapQueueName(name))
		if err != nil {
			var aerr *amqp.Error
			if errors.As(err, &aerr) && aerr.Code == amqp.NotFound {
				return 0, nil
			}
			return 0, err
		}
		return d, nil
	}, true
}

// runCaps prints the capability catalog (config/capabilities.yaml) for
// the model: one line per capability — where it can run, what it costs
// ("needs brain out" / "runs alongside you on cpu"), the current queue
// depth, and how long it usually takes. The lines are designed to be
// read, not parsed: the round preamble will feed them to the model as-is.
func runCaps(args []string) error {
	fs := flag.NewFlagSet("caps", flag.ExitOnError)
	file := fs.String("file", catalogDefaultPath(), "capability catalog file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: jarvis-mq caps [--file PATH]")
	}
	f, err := catalog.Load(*file)
	if err != nil {
		return err
	}
	depth, ok := capDepths()
	tw := tabwriter.NewWriter(os.Stdout, 8, 8, 2, ' ', 0)
	for _, name := range f.SortedNames() {
		c := f.Capabilities[name]
		depths := "-"
		if ok {
			if d, derr := depth(name); derr == nil {
				depths = strconv.Itoa(d)
			}
		}
		rt, rerr := c.TypicalRuntime()
		rts := "unknown"
		if rerr == nil {
			rts = rt.String()
		}
		fmt.Fprintf(tw, "%s\twhere=%s\tcost=%s\tdepth=%s\ttypical~%s\n",
			name, strings.Join(c.Locations(), ","), c.Cost(), depths, rts)
	}
	return tw.Flush()
}
