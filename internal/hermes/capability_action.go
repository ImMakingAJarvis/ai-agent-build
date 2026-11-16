// submit_capability (plan task EVB-32): chat becomes a capability job.
// "Describe the photos in folder X" classifies to submit_capability, the
// action publishes an envelope-v2 request to cap.<capability> with the
// reply-to on the Hermes-owned flow queue (CapReplyQueue), and the
// jarvis-chat daemon's durable consumer (WatchCapResults, wired in
// cmd/jarvis-chat) replies on Discord when the Result lands. The reply
// path is a consumer rather than a switchboard notify listener because a
// notify listener can only send its static Text — the result body (the
// per-image captions) has to travel with the reply. Full access only: a
// capability job spends owner money on the workers, and limited bots must
// not be able to start them.
package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// CapReplyQueue is the Hermes-owned flow queue that submit_capability
// Results land on. It is durable, so a Result that arrives while the
// daemon is down is still there when the watcher reconnects.
const CapReplyQueue = "flow.hermes"

// CapabilityAction builds the submit_capability action.
func CapabilityAction() Action {
	return Action{
		Key:              "submit_capability",
		RequiresFullAccess: true,
		Description:      "Run a capability job on the capability workers and reply on Discord when the result lands. Parameters: capability (dotted name — echo, image.describe, image.generate, audio.transcribe, llm.small, bench.run), path (the file or folder the worker reads), plus the capability's other inputs. Example: 'describe the photos in folder X' → capability=image.describe, path=X.",
		Run:              runSubmitCapability,
	}
}

// refParamNames are the parameter names the classifier uses for a file or
// folder the worker should read — all of them become InputRefs (the
// worker's primary input form), whichever the model picked.
var refParamNames = map[string]bool{
	"path": true, "folder": true, "file": true, "files": true, "input_ref": true,
}

// runSubmitCapability validates, builds the envelope-v2 request and
// publishes it to cap.<capability>. Validation happens before any broker
// contact, so a bad request fails fast without a connection.
func runSubmitCapability(_ context.Context, params map[string]string) (string, error) {
	capability := strings.TrimSpace(params["capability"])
	if capability == "" {
		return "", fmt.Errorf("submit_capability: the 'capability' parameter is required (e.g. image.describe)")
	}
	if !queue.ValidCapabilityName(capability) {
		return "", fmt.Errorf("submit_capability: %q is not a valid capability name (dotted lowercase, e.g. image.describe)", capability)
	}
	var refs []string
	input := map[string]any{}
	for k, v := range params {
		switch {
		case k == "capability", k == "corr", k == "out_dir", k == "deadline":
			// reserved: consumed below
		case refParamNames[k]:
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					refs = append(refs, p)
				}
			}
		default:
			input[k] = v
		}
	}
	if len(input) == 0 {
		input = nil
	}
	corr := strings.TrimSpace(params["corr"])
	if corr == "" {
		corr = "hm-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	outDir := strings.TrimSpace(params["out_dir"])
	if outDir == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			home = "/tmp"
		}
		// Under the shared artifacts root, visible from the worker
		// containers (they mount ~/.local/share/jarvis).
		outDir = filepath.Join(home, ".local", "share", "jarvis", "artifacts", "hermes-"+corr)
	}
	deadline := time.Now().Add(30 * time.Minute)
	if ds := strings.TrimSpace(params["deadline"]); ds != "" {
		t, err := queue.ParseDeadline(ds)
		if err != nil {
			return "", fmt.Errorf("submit_capability: deadline: %w", err)
		}
		deadline = t
	}
	m := queue.Message{
		ID:            "hermes-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Type:          "capability",
		Source:        "hermes",
		CreatedAt:     time.Now().UTC(),
		SchemaVersion: 2,
		Capability:    capability,
		CorrelationID: corr,
		ReplyTo:       CapReplyQueue,
		Input:         input,
		InputRefs:     refs,
		OutDir:        outDir,
		Deadline:      &deadline,
	}
	conn, err := queue.NewConn()
	if err != nil {
		return "The message broker is unreachable, so the capability job was not submitted. Ask me to try again in a moment.", nil
	}
	defer conn.Close()
	qname := queue.CapQueueName(capability)
	if err := conn.EnsureCapQueue(qname); err != nil {
		return "", fmt.Errorf("submit_capability: %w", err)
	}
	if err := conn.EnsureFlowQueue(CapReplyQueue); err != nil {
		return "", fmt.Errorf("submit_capability: %w", err)
	}
	if err := conn.Publish(m, qname+".default"); err != nil {
		return "", fmt.Errorf("submit_capability: publish: %w", err)
	}
	return fmt.Sprintf("Submitted the %s job (correlation %s); I will reply here as soon as the result lands.", capability, corr), nil
}

// capResultCap bounds the Discord reply (Discord caps a message at 2000
// characters; leave headroom for the status line).
const capResultCap = 1800

// FormatCapResult renders a capability Result for the Discord reply. It is
// capability-aware: an image.describe index.json artifact becomes one line
// per image ("file: caption"); anything else falls back to the worker's
// summary and artifact list.
func FormatCapResult(r queue.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** — %s (corr %s)\n", r.Capability, statusWord(r.Status), r.CorrelationID)
	switch r.Status {
	case queue.ResultOK:
		if s := strings.TrimSpace(r.Summary); s != "" {
			b.WriteString(s + "\n")
		}
		if line := describeIndexLine(r); line != "" {
			b.WriteString(line + "\n")
		}
		if d := dataLine(r); d != "" {
			b.WriteString(d + "\n")
		}
	case queue.ResultError:
		fmt.Fprintf(&b, "failed: %s\n", orDash(r.Error))
	case queue.ResultTimeout:
		fmt.Fprintf(&b, "timed out: %s\n", orDash(r.Error))
	case queue.ResultCancelled:
		b.WriteString("cancelled\n")
	default:
		fmt.Fprintf(&b, "status: %s\n", r.Status)
	}
	if r.Worker != "" {
		fmt.Fprintf(&b, "_worker: %s_", r.Worker)
	}
	return capText(b.String(), capResultCap)
}

// statusWord is the short human status for the reply's headline.
func statusWord(s string) string {
	switch s {
	case queue.ResultOK:
		return "done"
	case queue.ResultError:
		return "failed"
	case queue.ResultTimeout:
		return "timed out"
	case queue.ResultCancelled:
		return "cancelled"
	default:
		return s
	}
}

// describeIndexLine renders the per-image lines when the result is an
// image.describe index (one JSON artifact named index.json carries the
// captions — reading it is what turns "result landed" into the actual
// answer Nathan asked for).
func describeIndexLine(r queue.Result) string {
	for _, a := range r.Artifacts {
		if filepath.Base(a) != "index.json" {
			continue
		}
		b, err := os.ReadFile(a)
		if err != nil {
			return ""
		}
		var idx struct {
			Capability string `json:"capability"`
			Images     []struct {
				File    string   `json:"file"`
				Caption string   `json:"caption"`
				Tags    []string `json:"tags"`
			} `json:"images"`
		}
		if err := json.Unmarshal(b, &idx); err != nil || idx.Capability != "image.describe" {
			return ""
		}
		var sb strings.Builder
		for i, img := range idx.Images {
			if i >= 10 {
				fmt.Fprintf(&sb, "(and %d more)\n", len(idx.Images)-10)
				break
			}
			caption := strings.TrimSpace(img.Caption)
			caption = strings.Join(strings.Fields(caption), " ") // one line per image
			if len(caption) > 300 {
				caption = caption[:297] + "..."
			}
			name := filepath.Base(img.File)
			if tags := strings.Join(img.Tags, " "); tags != "" {
				fmt.Fprintf(&sb, "- %s: %s _(tags: %s)_\n", name, orDash(caption), tags)
			} else {
				fmt.Fprintf(&sb, "- %s: %s\n", name, orDash(caption))
			}
		}
		return strings.TrimRight(sb.String(), "\n")
	}
	return ""
}

// dataLine renders a small generic Data payload (echo, llm.small, …).
func dataLine(r queue.Result) string {
	if len(r.Data) == 0 {
		return ""
	}
	b, err := json.Marshal(r.Data)
	if err != nil {
		return ""
	}
	if len(b) > 400 {
		return fmt.Sprintf("data: %s…", b[:397])
	}
	return "data: " + string(b)
}

// orDash renders "" as "—" so a missing caption or error is visible, not blank.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// capText trims s to n characters, cutting on a line boundary when possible.
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, '\n'); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, "\n ") + " …"
}

// WatchCapResults consumes the queue queueName (the Hermes-owned result
// queue) and calls onResult for every Result that lands, until ctx is
// cancelled or the consumer dies. The consumer is the listener for
// submit_capability (see the file header): a switchboard notify listener
// can only send its static Text, so the reply path is a durable consumer
// that can read the result body. Messages that are not Results (a stray
// event published to the queue) are acked and skipped, not retried.
func WatchCapResults(ctx context.Context, conn *queue.Conn, onResult func(queue.Result), logf func(format string, args ...any)) error {
	return watchCapResultsOn(ctx, conn, CapReplyQueue, onResult, logf)
}

// watchCapResultsOn is WatchCapResults on an explicit queue name (the
// tests use a throwaway queue so they cannot steal a real reply).
func watchCapResultsOn(ctx context.Context, conn *queue.Conn, queueName string, onResult func(queue.Result), logf func(format string, args ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if onResult == nil {
		return errors.New("hermes: WatchCapResults needs an onResult callback")
	}
	if err := conn.EnsureFlowQueue(queueName); err != nil {
		return fmt.Errorf("hermes: ensure %s: %w", queueName, err)
	}
	ch, err := conn.NewChannel()
	if err != nil {
		return fmt.Errorf("hermes: channel: %w", err)
	}
	defer ch.Close()
	msgs, err := ch.Consume(queueName, "hermes-cap-results", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("hermes: consume %s: %w", queueName, err)
	}
	logf("hermes: watching %s for capability results", queueName)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-msgs:
			if !ok {
				return fmt.Errorf("hermes: cap-results consumer on %s closed", queueName)
			}
			res, err := queue.DecodeResult(d.Body)
			if err != nil || res.Status == "" {
				logf("hermes: skipping a non-result message on %s: %v", queueName, err)
				_ = d.Ack(false)
				continue
			}
			_ = d.Ack(false)
			onResult(res)
		}
	}
}
