// Package bridge is the core of the plane-bridge service (P4 of
// docs/work-board/PLAN.md). It receives Plane webhooks, verifies their
// HMAC-SHA256 signature, ignores events made by the Jarvis bot (so the
// bridge and the bot never loop), and logs every event it acts on.
//
// Board changes do not act on the board directly: an item "in a trigger
// state" becomes an agent job in the agentjob store, which the agent-run
// loop picks up. The two trigger states (per PLAN.md P4):
//
//   - plan_epic: an item with the `epic` label and a `plan:<model>` label
//     still in the Backlog state → a planning job (Phase B handles it).
//   - run_task: a sub-item (has a parent) in the Ready state with no job
//     yet → an execution job (Phase C handles it).
//
// The same check runs on every webhook for the watched project and as a
// catch-up sync over the whole project on every start, so changes made
// while the bridge was down (webhook lost) are picked up on the next boot.
package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

// Trigger names recorded in queued job specs.
const (
	TriggerPlan = "plan_epic"
	TriggerRun  = "run_task"
)

// Config configures a Bridge.
type Config struct {
	Plane *plane.Client
	Store *agentjob.Store
	Log   *slog.Logger
	// ProjectID is the project whose items are checked for triggers.
	ProjectID string
	// ProjectName is the project identifier for human-readable job titles
	// (e.g. "AAB"); may be empty.
	ProjectName string
	// WebhookURL is the full URL Plane posts events to (must match the
	// registered webhook).
	WebhookURL string
	// SecretFile stores the webhook's HMAC secret (created with the
	// webhook, 0600). The API never re-reveals it on list/retrieve.
	SecretFile string
	// BotEmail is the Jarvis bot account (e.g. jarvis-bot2@nathan.homes);
	// events whose actor matches it are ignored.
	BotEmail string
	// BoardURL is the Plane base URL, recorded in job specs.
	BoardURL string
	// Workspace is the Plane workspace slug, recorded in job specs.
	Workspace string
	// EventsFile is an optional JSONL file that receives one line per
	// event the bridge acted on (not bot events, not bad signatures).
	EventsFile string
}

// Bridge verifies, logs and routes Plane webhooks and runs the trigger
// checks. It is safe for concurrent use.
type Bridge struct {
	cfg Config

	mu        sync.Mutex
	secretKey []byte            // loaded from cfg.SecretFile
	labels    map[string]string // label id → name
	states    map[string]string // state id → name

	evMu   sync.Mutex
	events *os.File

	wg sync.WaitGroup // in-flight trigger checks
}

// New builds a Bridge. cfg.Plane, cfg.Store, cfg.ProjectID,
// cfg.WebhookURL and cfg.SecretFile are required.
func New(cfg Config) (*Bridge, error) {
	if cfg.Plane == nil {
		return nil, errors.New("bridge: Plane client is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("bridge: job store is required")
	}
	if cfg.ProjectID == "" || cfg.WebhookURL == "" || cfg.SecretFile == "" {
		return nil, errors.New("bridge: ProjectID, WebhookURL and SecretFile are required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	b := &Bridge{cfg: cfg}
	if cfg.EventsFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.EventsFile), 0o755); err != nil {
			return nil, fmt.Errorf("bridge: events file dir: %w", err)
		}
		f, err := os.OpenFile(cfg.EventsFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("bridge: opening events file: %w", err)
		}
		b.events = f
	}
	return b, nil
}

// Close flushes and closes the event log.
func (b *Bridge) Close() error {
	b.evMu.Lock()
	defer b.evMu.Unlock()
	if b.events != nil {
		return b.events.Close()
	}
	return nil
}

// --- webhook registration -----------------------------------------------------------

// EnsureWebhook makes sure the workspace has an active webhook pointing at
// cfg.WebhookURL subscribed to issue and issue_comment events, and that a
// usable HMAC secret is in cfg.SecretFile. It is idempotent:
//
//   - webhook exists + secret file present  → nothing happens
//   - webhook exists + secret file missing  → the secret is regenerated
//     (the API never re-reveals the old one) and stored
//   - webhook missing                        → it is created and the
//     returned secret is stored
func (b *Bridge) EnsureWebhook(ctx context.Context) error {
	ws, err := b.cfg.Plane.ListWebhooks(ctx)
	if err != nil {
		return fmt.Errorf("bridge: listing webhooks: %w", err)
	}
	var found *plane.Webhook
	for i := range ws {
		if ws[i].URL == b.cfg.WebhookURL {
			found = &ws[i]
			break
		}
	}
	if found == nil {
		w, err := b.cfg.Plane.CreateWebhook(ctx, b.cfg.WebhookURL, "issue", "issue_comment")
		if err != nil {
			return fmt.Errorf("bridge: creating webhook: %w", err)
		}
		if err := b.saveSecret(w.SecretKey); err != nil {
			return err
		}
		b.cfg.Log.Info("webhook registered", "url", b.cfg.WebhookURL, "id", w.ID)
		return nil
	}
	if sec, err := b.secret(); err == nil && len(sec) > 0 {
		return nil
	}
	// No stored secret: the API will not show us the existing one, so
	// rotate and store the fresh one.
	w, err := b.cfg.Plane.RegenerateWebhookSecret(ctx, found.ID)
	if err != nil {
		return fmt.Errorf("bridge: regenerating webhook secret: %w", err)
	}
	if err := b.saveSecret(w.SecretKey); err != nil {
		return err
	}
	b.cfg.Log.Warn("webhook secret was missing and has been regenerated", "id", found.ID)
	return nil
}

func (b *Bridge) saveSecret(secret string) error {
	if secret == "" {
		return errors.New("bridge: Plane returned no webhook secret")
	}
	if err := os.MkdirAll(filepath.Dir(b.cfg.SecretFile), 0o755); err != nil {
		return fmt.Errorf("bridge: secret dir: %w", err)
	}
	if err := os.WriteFile(b.cfg.SecretFile, []byte(secret+"\n"), 0o600); err != nil {
		return fmt.Errorf("bridge: writing secret file: %w", err)
	}
	b.mu.Lock()
	b.secretKey = []byte(strings.TrimSpace(secret))
	b.mu.Unlock()
	return nil
}

// secret returns the HMAC secret, loading it from cfg.SecretFile on first
// use.
func (b *Bridge) secret() ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.secretKey) > 0 {
		return b.secretKey, nil
	}
	raw, err := os.ReadFile(b.cfg.SecretFile)
	if err != nil {
		return nil, fmt.Errorf("bridge: webhook secret not set: %w", err)
	}
	b.secretKey = []byte(strings.TrimSpace(string(raw)))
	return b.secretKey, nil
}

// --- webhook handling ----------------------------------------------------------------

// Payload is the Plane webhook body (see bgtasks/webhook_task.py in the
// Plane source: the HMAC in X-Plane-Signature covers exactly these bytes).
type Payload struct {
	Event         string    `json:"event"`
	Action        string    `json:"action"` // created, updated, deleted
	WebhookID     string    `json:"webhook_id"`
	WorkspaceID   string    `json:"workspace_id"`
	WorkspaceSlug string    `json:"workspace_slug"`
	Data          *Data     `json:"data"`
	Activity      *Activity `json:"activity"`
}

// Data is the serialized object the event is about. This Plane build sends
// full nested serializers in the webhook body — state and labels are
// objects and the project reference is "project", not bare ids (verified
// against a live delivery). On delete events Plane sends only the id.
type Data struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	ProjectID  string     `json:"project"`
	State      *StateRef  `json:"state"`
	Parent     string     `json:"parent"`
	Labels     []LabelRef `json:"labels"`
	SequenceID int        `json:"sequence_id"`
}

// StateRef is the state serializer embedded in a webhook data object.
type StateRef struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group"`
}

// LabelRef is a label serializer embedded in a webhook data object.
type LabelRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Activity describes what changed and who did it.
type Activity struct {
	Field *string `json:"field"`
	Actor *Actor  `json:"actor"`
}

// Actor is the user who made the change (UserLiteSerializer).
type Actor struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// HandleWebhook is the http.Handler for the webhook endpoint. It verifies
// the signature before anything else, ignores bot events, logs the rest,
// and runs the trigger check for issue events in the watched project.
// It always answers quickly: the trigger check runs in the background.
func (b *Bridge) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := readBody(r, 1<<20)
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	sig := r.Header.Get("X-Plane-Signature")
	if !b.validSignature(body, sig) {
		b.cfg.Log.Warn("webhook rejected: bad signature",
			"delivery", r.Header.Get("X-Plane-Delivery"), "event", r.Header.Get("X-Plane-Event"))
		http.Error(w, "bad signature", http.StatusForbidden)
		return
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		b.cfg.Log.Warn("webhook rejected: bad payload",
			"delivery", r.Header.Get("X-Plane-Delivery"), "err", err)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if b.isBot(p) {
		b.cfg.Log.Info("webhook ignored (bot event)",
			"event", p.Event, "action", p.Action, "actor", p.actorEmail())
		w.WriteHeader(http.StatusOK)
		return
	}
	b.logEvent(&p)
	if p.Event == "issue" && p.Data != nil && p.Data.ProjectID == b.cfg.ProjectID {
		itemID := p.Data.ID
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		b.wg.Add(1)
		go func() {
			defer cancel()
			defer b.wg.Done()
			if n, err := b.CheckItem(ctx, itemID); err != nil {
				b.cfg.Log.Error("trigger check failed", "item", itemID, "err", err)
			} else if n > 0 {
				b.cfg.Log.Info("trigger check queued jobs", "item", itemID, "queued", n)
			}
		}()
	}
	w.WriteHeader(http.StatusOK)
}

// Wait blocks until in-flight trigger checks finish (for tests and
// shutdown).
func (b *Bridge) Wait() { b.wg.Wait() }

func readBody(r *http.Request, limit int64) ([]byte, error) {
	defer r.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > limit {
		return nil, errors.New("body exceeds limit")
	}
	return buf, nil
}

// validSignature checks X-Plane-Signature: the hex HMAC-SHA256 of the raw
// body keyed with the webhook secret.
func (b *Bridge) validSignature(body []byte, sig string) bool {
	sec, err := b.secret()
	if err != nil || len(sec) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, sec)
	mac.Write(body)
	want := mac.Sum(nil)
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	return hmac.Equal(want, got)
}

// isBot reports whether the event's actor is the Jarvis bot account.
func (b *Bridge) isBot(p Payload) bool {
	if b.cfg.BotEmail == "" || p.Activity == nil || p.Activity.Actor == nil {
		return false
	}
	return strings.EqualFold(p.Activity.Actor.Email, b.cfg.BotEmail)
}

func (p *Payload) actorEmail() string {
	if p.Activity != nil && p.Activity.Actor != nil {
		return p.Activity.Actor.Email
	}
	return ""
}

// logEvent appends one JSONL line to the events file and logs it.
func (b *Bridge) logEvent(p *Payload) {
	line := map[string]any{
		"ts":        time.Now().UTC().Format(time.RFC3339),
		"event":     p.Event,
		"action":    p.Action,
		"actor":     p.actorEmail(),
		"delivery":  p.WebhookID,
		"workspace": p.WorkspaceSlug,
	}
	if p.Data != nil {
		line["item_id"] = p.Data.ID
		line["item_name"] = p.Data.Name
		line["project_id"] = p.Data.ProjectID
	}
	if p.Activity != nil && p.Activity.Field != nil {
		line["field"] = *p.Activity.Field
	}
	b.cfg.Log.Info("webhook event",
		"event", p.Event, "action", p.Action, "actor", p.actorEmail(),
		"item", p.itemName(), "field", p.fieldName())
	b.evMu.Lock()
	defer b.evMu.Unlock()
	if b.events == nil {
		return
	}
	raw, err := json.Marshal(line)
	if err != nil {
		return
	}
	if _, err := b.events.Write(append(raw, '\n')); err != nil {
		b.cfg.Log.Warn("writing events file failed", "err", err)
	}
}

func (p *Payload) itemName() string {
	if p.Data == nil {
		return ""
	}
	return p.Data.Name
}

func (p *Payload) fieldName() string {
	if p.Activity == nil || p.Activity.Field == nil {
		return ""
	}
	return *p.Activity.Field
}

// --- trigger checks -------------------------------------------------------------------

// CheckItem re-reads the item from the API and queues jobs for any trigger
// state it is in. It is idempotent: already-queued items queue nothing.
func (b *Bridge) CheckItem(ctx context.Context, itemID string) (int, error) {
	item, err := b.cfg.Plane.GetWorkItem(ctx, b.cfg.ProjectID, itemID)
	if err != nil {
		return 0, err
	}
	return b.evaluate(ctx, item)
}

// CatchUp runs the trigger check over every item in the watched project.
// It is called on every start; it returns how many jobs were queued.
func (b *Bridge) CatchUp(ctx context.Context) (int, error) {
	b.invalidateMeta() // fresh board metadata after downtime
	items, err := b.cfg.Plane.ListWorkItems(ctx, b.cfg.ProjectID)
	if err != nil {
		return 0, fmt.Errorf("bridge: listing work items: %w", err)
	}
	total := 0
	for i := range items {
		n, err := b.evaluate(ctx, &items[i])
		if err != nil {
			b.cfg.Log.Warn("catch-up: item check failed", "item", items[i].ID, "err", err)
			continue
		}
		total += n
	}
	b.cfg.Log.Info("catch-up sync done", "items", len(items), "queued", total)
	return total, nil
}

// evaluate decides whether item is in a trigger state and queues the
// matching job(s).
func (b *Bridge) evaluate(ctx context.Context, item *plane.WorkItem) (int, error) {
	names, err := b.labelNames(ctx, item)
	if err != nil {
		return 0, err
	}
	stateName, err := b.stateName(ctx, item)
	if err != nil {
		return 0, err
	}
	var queued int
	modelID := modelLabel(names)
	if hasLabel(names, "epic") && modelID != "" && strings.EqualFold(stateName, "Backlog") {
		if b.enqueue(ctx, TriggerPlan, item, modelID, names) {
			queued++
		}
	}
	if item.Parent != "" && strings.EqualFold(stateName, "Ready") {
		if b.enqueue(ctx, TriggerRun, item, "", names) {
			queued++
		}
	}
	return queued, nil
}

// modelLabel returns the model id from a plan:<id> label, or "".
func modelLabel(names []string) string {
	for _, n := range names {
		if strings.HasPrefix(n, "plan:") {
			return strings.TrimPrefix(n, "plan:")
		}
	}
	return ""
}

func hasLabel(names []string, want string) bool {
	for _, n := range names {
		if strings.EqualFold(n, want) {
			return true
		}
	}
	return false
}

// enqueue stores the job for this trigger+item, or reports it already
// exists. Job ids are deterministic (plane-<trigger>-<item id>), so
// redelivered webhooks and repeated catch-ups never duplicate.
func (b *Bridge) enqueue(ctx context.Context, trigger string, item *plane.WorkItem, modelID string, labelNames []string) bool {
	id := "plane-" + trigger + "-" + item.ID
	if _, err := b.cfg.Store.Get(id); err == nil {
		return false // a job for this trigger+item already exists
	}
	spec := agentjob.Spec{
		ID:       id,
		Title:    b.title(trigger, item, modelID),
		Class:    agentjob.ClassBackground,
		Priority: 100, // front of the background band; conversation work still
		// runs first (it is a different system)
		Source:       "plane-bridge",
		CreatedAt:    time.Now().UTC(),
		Instructions: instructions(trigger, b.cfg, item, modelID, labelNames),
	}
	added, err := b.cfg.Store.Ingest(spec)
	if err != nil {
		b.cfg.Log.Error("queuing job failed", "job", id, "err", err)
		return false
	}
	if added {
		b.cfg.Log.Info("queued job from trigger state", "job", id, "trigger", trigger,
			"item", item.ID, "name", item.Name, "model", modelID)
	}
	return added
}

func (b *Bridge) title(trigger string, item *plane.WorkItem, modelID string) string {
	code := b.cfg.ProjectName
	if code == "" {
		code = "plane"
	}
	switch trigger {
	case TriggerPlan:
		return fmt.Sprintf("Plane: plan epic %s-%d (%s) with %s", code, item.SequenceID, item.Name, modelID)
	case TriggerRun:
		return fmt.Sprintf("Plane: run task %s-%d (%s)", code, item.SequenceID, item.Name)
	}
	return fmt.Sprintf("Plane: %s %s-%d (%s)", trigger, code, item.SequenceID, item.Name)
}

// instructions is the JSON body of the queued job — the contract later
// phases (B1, C1) consume.
func instructions(trigger string, cfg Config, item *plane.WorkItem, modelID string, labelNames []string) string {
	body := map[string]any{
		"trigger": trigger,
		"board": map[string]string{
			"url":        cfg.BoardURL,
			"workspace":  cfg.Workspace,
			"project":    cfg.ProjectName,
			"project_id": cfg.ProjectID,
		},
		"item": map[string]any{
			"id":          item.ID,
			"name":        item.Name,
			"sequence_id": item.SequenceID,
			"parent":      item.Parent,
			"state":       item.State,
			"labels":      labelNames,
		},
	}
	if modelID != "" {
		body["model_id"] = modelID
	}
	raw, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"trigger": %q, "item_id": %q}`, trigger, item.ID)
	}
	return string(raw)
}

// --- board metadata (label/state name maps) -------------------------------------------

// labelNames returns the label names of item, using a project-wide cache
// that is refreshed when an item references an unknown label.
func (b *Bridge) labelNames(ctx context.Context, item *plane.WorkItem) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.labels == nil {
		if err := b.refreshLocked(ctx); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(item.Labels))
	missing := false
	for _, id := range item.Labels {
		if n, ok := b.labels[id]; ok {
			names = append(names, n)
		} else {
			missing = true
			break
		}
	}
	if missing {
		if err := b.refreshLocked(ctx); err != nil {
			return nil, err
		}
		names = make([]string, 0, len(item.Labels))
		for _, id := range item.Labels {
			if n, ok := b.labels[id]; ok {
				names = append(names, n)
			}
		}
	}
	return names, nil
}

// stateName returns the display name of the item's state.
func (b *Bridge) stateName(ctx context.Context, item *plane.WorkItem) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.states == nil {
		if err := b.refreshLocked(ctx); err != nil {
			return "", err
		}
	}
	if n, ok := b.states[item.State]; ok {
		return n, nil
	}
	// unknown state: refresh once (the project's states may have changed)
	if err := b.refreshLocked(ctx); err != nil {
		return "", err
	}
	if n, ok := b.states[item.State]; ok {
		return n, nil
	}
	return "", fmt.Errorf("bridge: unknown state id %q", item.State)
}

func (b *Bridge) invalidateMeta() {
	b.mu.Lock()
	b.labels = nil
	b.states = nil
	b.mu.Unlock()
}

// refreshLocked reloads the project's labels and states. Callers hold mu.
func (b *Bridge) refreshLocked(ctx context.Context) error {
	labels, err := b.cfg.Plane.ListLabels(ctx, b.cfg.ProjectID)
	if err != nil {
		return fmt.Errorf("bridge: listing labels: %w", err)
	}
	states, err := b.cfg.Plane.ListStates(ctx, b.cfg.ProjectID)
	if err != nil {
		return fmt.Errorf("bridge: listing states: %w", err)
	}
	b.labels = make(map[string]string, len(labels))
	for _, l := range labels {
		b.labels[l.ID] = l.Name
	}
	b.states = make(map[string]string, len(states))
	for _, s := range states {
		b.states[s.ID] = s.Name
	}
	return nil
}
