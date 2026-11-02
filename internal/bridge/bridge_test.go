package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/fit"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// fakePlane is a minimal in-memory Plane server covering exactly the
// endpoints the bridge uses: project labels and states, work items (get +
// list) on the v1 API, and workspace webhooks (list/create/regenerate —
// the secret is only revealed on create/regenerate, like the real API)
// on the session API, behind cookie + CSRF auth like the real build.
const (
	fakeEmail  = "bot@fake"
	fakePasswd = "pw"
	fakeCSRF   = "csrf"
	fakeSess   = "fakesess"
	sessionWS  = "/api/workspaces/testws/"
)

type fakePlane struct {
	mu         sync.Mutex
	labels     map[string]string                    // id -> name (shared across projects)
	states     map[string]string                    // id -> name
	items      map[string]map[string]map[string]any // project id -> item id -> item
	comments   []fakeComment
	stateMoves []fakeStateMove
	whID       string
	whURL      string
	whSecret   string
	created    int // webhook creations
	regen      int // secret regenerations
}

// fakeComment is a comment captured by the fake server.
type fakeComment struct {
	project string
	item    string
	html    string
}

// fakeStateMove is a state change applied through the fake SetState API.
type fakeStateMove struct {
	project string
	item    string
	state   string // state id
}

// Comments returns the comments captured so far.
func (f *fakePlane) Comments() []fakeComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeComment(nil), f.comments...)
}

// StateMoves returns the state changes applied so far, in order.
func (f *fakePlane) StateMoves() []fakeStateMove {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeStateMove(nil), f.stateMoves...)
}

func newFakePlane() *fakePlane {
	return &fakePlane{
		labels: map[string]string{"lbl-epic": "epic", "lbl-model": "plan:opus"},
		states: map[string]string{"st-backlog": "Backlog", "st-planning": "Planning", "st-ready": "Ready"},
		items:  map[string]map[string]map[string]any{},
	}
}

// sessionCookie reads the session cookie off the request.
func (f *fakePlane) sessionCookie(r *http.Request) string {
	if c, err := r.Cookie("session-id"); err == nil {
		return c.Value
	}
	return ""
}

// setItem stores a work item in projectID with the given state id and
// label ids.
func (f *fakePlane) setItem(projectID, id, name, stateID, parent string, seq int, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.items[projectID] == nil {
		f.items[projectID] = map[string]map[string]any{}
	}
	f.items[projectID][id] = map[string]any{
		"id": id, "name": name, "state": stateID, "parent": parent,
		"labels": labels, "sequence_id": seq,
	}
}

func (f *fakePlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// --- session API: auth + workspace webhooks ---
	switch r.URL.Path {
	case "/auth/get-csrf-token/":
		json.NewEncoder(w).Encode(map[string]string{"csrf_token": fakeCSRF})
		return
	case "/auth/sign-in/":
		r.ParseForm()
		if r.PostForm.Get("email") != fakeEmail || r.PostForm.Get("password") != fakePasswd {
			http.Error(w, "bad credentials", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session-id", Value: fakeSess, Path: "/"})
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if strings.HasPrefix(r.URL.Path, sessionWS) {
		if f.sessionCookie(r) != fakeSess {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"detail": "Authentication credentials were not provided."})
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-CSRFToken") != fakeCSRF {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("CSRF Failed"))
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, sessionWS)
		if strings.HasPrefix(rest, "webhooks") {
			f.routeWebhook(w, r, strings.TrimPrefix(rest, "webhooks"))
			return
		}
		http.Error(w, "unrouted session path", http.StatusNotFound)
		return
	}
	// --- v1 external API: project routes ---
	const prefix = "/api/v1/workspaces/testws/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "wrong workspace", http.StatusNotFound)
		return
	}
	rest := r.URL.Path[len(prefix):]
	proj, rest2, _ := strings.Cut(strings.TrimPrefix(rest, "projects/"), "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case rest2 == "labels" || rest2 == "labels/":
		var out []map[string]string
		for id, name := range f.labels {
			out = append(out, map[string]string{"id": id, "name": name})
		}
		json.NewEncoder(w).Encode(map[string]any{"results": out, "count": len(out)})
	case rest2 == "states" || rest2 == "states/":
		var out []map[string]string
		for id, name := range f.states {
			out = append(out, map[string]string{"id": id, "name": name})
		}
		json.NewEncoder(w).Encode(map[string]any{"results": out, "count": len(out)})
	case strings.HasPrefix(rest2, "work-items"):
		rest3 := strings.Trim(strings.TrimPrefix(rest2, "work-items"), "/")
		id, trail, _ := strings.Cut(rest3, "/")
		if trail == "comments" {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			f.comments = append(f.comments, fakeComment{project: proj, item: id, html: body["comment_html"]})
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"id": "cm-" + strconv.Itoa(len(f.comments))})
			return
		}
		if trail == "" && r.Method == http.MethodPatch {
			// SetState: PATCH the item with {"state": <state id>}
			it, ok := f.items[proj][id]
			if !ok {
				http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
				return
			}
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["state"] != "" {
				f.stateMoves = append(f.stateMoves, fakeStateMove{project: proj, item: id, state: body["state"]})
				it["state"] = body["state"]
			}
			json.NewEncoder(w).Encode(it)
			return
		}
		items := f.items[proj]
		if id == "" {
			all := make([]map[string]any, 0, len(items))
			for _, it := range items {
				all = append(all, it)
			}
			json.NewEncoder(w).Encode(all)
			return
		}
		it, ok := items[id]
		if !ok {
			http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(it)
	}
}

func (f *fakePlane) routeWebhook(w http.ResponseWriter, r *http.Request, rest string) {
	rest = strings.Trim(rest, "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case rest == "":
		switch r.Method {
		case http.MethodGet:
			if f.whID == "" {
				json.NewEncoder(w).Encode([]map[string]string{})
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{{
				"id": f.whID, "url": f.whURL, "is_active": true,
			}})
		case http.MethodPost:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			url, _ := body["url"].(string)
			f.created++
			f.whID = "wh-1"
			f.whURL = url
			f.whSecret = "s3cret"
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"id": f.whID, "url": url, "is_active": true,
				"secret_key": f.whSecret, "issue": true,
			})
		}
	case strings.HasSuffix(rest, "regenerate"):
		f.regen++
		f.whSecret = "rotated-" + f.whSecret
		json.NewEncoder(w).Encode(map[string]any{
			"id":         strings.TrimSuffix(rest, "/regenerate"),
			"secret_key": f.whSecret,
		})
	default:
		http.Error(w, "unrouted webhook path", http.StatusNotFound)
	}
}

// sign returns the X-Plane-Signature for body with the given secret.
func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// eventBody builds a webhook payload for an issue update on itemID, in the
// shape this Plane build actually sends (nested state/label objects,
// "project" as the project reference).
func eventBody(itemID, projectID, actorEmail, field string) []byte {
	p := map[string]any{
		"event": "issue", "action": "update", "webhook_id": "wh-1",
		"workspace_id": "ws-1", "workspace_slug": "testws",
		"data": map[string]any{
			"id": itemID, "name": "some item", "project": projectID,
			"state":  map[string]any{"id": "st-1", "name": "Backlog", "group": "backlog"},
			"labels": []any{},
		},
	}
	if actorEmail != "" {
		p["activity"] = map[string]any{
			"field": field,
			"actor": map[string]string{"id": "u-1", "email": actorEmail},
		}
	}
	b, _ := json.Marshal(p)
	return b
}

// newBridge wires a Bridge against a fresh fake Plane and job store. The
// fake already has the webhook registered and the secret stored, so tests
// can sign payloads without calling EnsureWebhook first.
func newBridge(t *testing.T, botEmail string) (*Bridge, *fakePlane, *agentjob.Store, string) {
	t.Helper()
	f := newFakePlane()
	f.whID = "wh-1"
	f.whURL = "http://bridge:8102/webhook/plane"
	f.whSecret = "s3cret"
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := plane.New(plane.Config{
		BaseURL: srv.URL, APIKey: "testkey", Workspace: "testws",
		Email: fakeEmail, Password: fakePasswd,
	})
	if err != nil {
		t.Fatalf("plane.New: %v", err)
	}
	store, err := agentjob.Open(t.TempDir())
	if err != nil {
		t.Fatalf("agentjob.Open: %v", err)
	}
	dir := t.TempDir()
	b, err := New(Config{
		Plane:        c,
		Store:        store,
		Projects:     []string{"proj-1"},
		ProjectNames: map[string]string{"proj-1": "AAB"},
		WebhookURL:   "http://bridge:8102/webhook/plane",
		SecretFile:   filepath.Join(dir, "secret"),
		BotEmail:     botEmail,
		BoardURL:     srv.URL,
		Workspace:    "testws",
		EventsFile:   filepath.Join(dir, "events.jsonl"),
	})
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	if err := b.saveSecret("s3cret"); err != nil {
		t.Fatalf("saveSecret: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b, f, store, dir
}

// postWebhook sends a signed payload and returns the response status.
func postWebhook(t *testing.T, b *Bridge, body []byte, sig string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/plane", strings.NewReader(string(body)))
	if sig != "" {
		req.Header.Set("X-Plane-Signature", sig)
	}
	rec := httptest.NewRecorder()
	b.HandleWebhook(rec, req)
	return rec.Code
}

func eventLines(t *testing.T, dir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("reading events file: %v", err)
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func TestHandleWebhookRejectsBadSignature(t *testing.T) {
	b, _, store, dir := newBridge(t, "bot@x")
	body := eventBody("wi-1", "proj-1", "human@x", "labels")

	if code := postWebhook(t, b, body, ""); code != http.StatusForbidden {
		t.Errorf("missing signature: status %d, want 403", code)
	}
	if code := postWebhook(t, b, body, sign("wrong-secret", body)); code != http.StatusForbidden {
		t.Errorf("wrong signature: status %d, want 403", code)
	}
	if _, err := store.Get("plane-plan_epic-wi-1"); err == nil {
		t.Error("rejected webhook must not queue jobs")
	}
	if n := eventLines(t, dir); n != 0 {
		t.Errorf("rejected webhook wrote %d event lines, want 0", n)
	}
}

// TestHandleWebhookBotEventIgnored is half of the P4 acceptance criterion:
// a bot edit of a trigger item is ignored — no log line, no job.
func TestHandleWebhookBotEventIgnored(t *testing.T) {
	b, f, store, dir := newBridge(t, "jarvis-bot@x")
	f.setItem("proj-1", "wi-1", "epic", "st-backlog", "", 1, "lbl-epic", "lbl-model")

	body := eventBody("wi-1", "proj-1", "Jarvis-Bot@x", "labels") // case-insensitive match
	if code := postWebhook(t, b, body, sign("s3cret", body)); code != http.StatusOK {
		t.Fatalf("bot event: status %d, want 200", code)
	}
	b.Wait()
	if n := eventLines(t, dir); n != 0 {
		t.Errorf("bot event wrote %d event lines, want 0", n)
	}
	if _, err := store.Get("plane-plan_epic-wi-1"); err == nil {
		t.Error("bot event must not queue a job")
	}
}

// TestHandleWebhookHumanEventLogsAndQueues is the other half: a human edit
// of the same item is logged and queues the plan job.
func TestHandleWebhookHumanEventLogsAndQueues(t *testing.T) {
	b, f, store, dir := newBridge(t, "jarvis-bot@x")
	f.setItem("proj-1", "wi-1", "Ship the widget", "st-backlog", "", 7, "lbl-epic", "lbl-model")

	body := eventBody("wi-1", "proj-1", "nathan@x", "labels")
	if code := postWebhook(t, b, body, sign("s3cret", body)); code != http.StatusOK {
		t.Fatalf("human event: status %d, want 200", code)
	}
	b.Wait()
	if n := eventLines(t, dir); n != 1 {
		t.Errorf("human event wrote %d event lines, want 1", n)
	}
	job, err := store.Get("plane-plan_epic-wi-1")
	if err != nil {
		t.Fatalf("human edit did not queue the plan job: %v", err)
	}
	if !strings.Contains(job.Title, "plan epic AAB-7") {
		t.Errorf("job title = %q, want a plan epic AAB-7 title", job.Title)
	}
	var inst map[string]any
	if err := json.Unmarshal([]byte(job.Instructions), &inst); err != nil {
		t.Fatalf("job instructions are not JSON: %v", err)
	}
	if inst["model_id"] != "opus" {
		t.Errorf("instructions model_id = %v, want opus", inst["model_id"])
	}
}

// TestHandleWebhookOtherProjectNotChecked: events for other projects are
// logged but never run the trigger check.
func TestHandleWebhookOtherProjectNotChecked(t *testing.T) {
	b, f, store, dir := newBridge(t, "jarvis-bot@x")
	f.setItem("proj-1", "wi-9", "epic", "st-backlog", "", 1, "lbl-epic", "lbl-model")

	body := eventBody("wi-9", "proj-other", "nathan@x", "labels")
	if code := postWebhook(t, b, body, sign("s3cret", body)); code != http.StatusOK {
		t.Fatalf("other-project event: status %d, want 200", code)
	}
	b.Wait()
	if n := eventLines(t, dir); n != 1 {
		t.Errorf("event lines = %d, want 1 (logged but not acted on)", n)
	}
	if _, err := store.Get("plane-plan_epic-wi-9"); err == nil {
		t.Error("other-project item must not queue a job")
	}
}

// TestCatchUpQueuesTriggerJobs covers the catch-up acceptance criterion:
// trigger items that exist when the bridge starts are queued, and a second
// catch-up queues nothing (idempotent).
func TestCatchUpQueuesTriggerJobs(t *testing.T) {
	b, f, store, _ := newBridge(t, "jarvis-bot@x")
	// in trigger states:
	f.setItem("proj-1", "epic-1", "Big epic", "st-backlog", "", 1, "lbl-epic", "lbl-model")
	f.setItem("proj-1", "sub-1", "Sub task", "st-ready", "epic-1", 2)
	// not in trigger states:
	f.setItem("proj-1", "plain-1", "Plain backlog item", "st-backlog", "", 3)                   // no labels
	f.setItem("proj-1", "noplan-1", "Epic without plan label", "st-backlog", "", 4, "lbl-epic") // no plan:<model>
	f.setItem("proj-1", "orphan-1", "Ready but no parent", "st-ready", "", 5)                   // not a sub-item

	n, err := b.CatchUp(context.Background())
	if err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	if n != 2 {
		t.Fatalf("CatchUp queued %d jobs, want 2", n)
	}
	for _, id := range []string{"plane-plan_epic-epic-1", "plane-run_task-sub-1"} {
		if _, err := store.Get(id); err != nil {
			t.Errorf("missing job %s: %v", id, err)
		}
	}
	for _, id := range []string{"plane-plan_epic-plain-1", "plane-plan_epic-noplan-1", "plane-run_task-orphan-1"} {
		if _, err := store.Get(id); err == nil {
			t.Errorf("job %s should not exist (not a trigger state)", id)
		}
	}

	// second catch-up (e.g. restart) queues nothing new
	n, err = b.CatchUp(context.Background())
	if err != nil {
		t.Fatalf("second CatchUp: %v", err)
	}
	if n != 0 {
		t.Errorf("second CatchUp queued %d jobs, want 0 (idempotent)", n)
	}
}

// TestEnsureWebhook covers the idempotent registration: create once, no-op
// while the secret is stored, regenerate when the secret file is lost.
func TestEnsureWebhook(t *testing.T) {
	b, f, _, dir := newBridge(t, "")
	// start from a clean slate: no webhook in the fake, no secret file
	f.mu.Lock()
	f.whID, f.whSecret, f.created, f.regen = "", "", 0, 0
	f.mu.Unlock()
	os.Remove(filepath.Join(dir, "secret"))
	b.mu.Lock()
	b.secretKey = nil
	b.mu.Unlock()

	if err := b.EnsureWebhook(context.Background()); err != nil {
		t.Fatalf("EnsureWebhook (create): %v", err)
	}
	if f.created != 1 {
		t.Fatalf("webhook creations = %d, want 1", f.created)
	}
	sec, err := os.ReadFile(filepath.Join(dir, "secret"))
	if err != nil {
		t.Fatalf("secret file not written: %v", err)
	}
	if strings.TrimSpace(string(sec)) != "s3cret" {
		t.Errorf("stored secret = %q, want s3cret", strings.TrimSpace(string(sec)))
	}

	// second call: nothing happens
	if err := b.EnsureWebhook(context.Background()); err != nil {
		t.Fatalf("EnsureWebhook (no-op): %v", err)
	}
	if f.created != 1 || f.regen != 0 {
		t.Errorf("second EnsureWebhook: created=%d regen=%d, want 1/0", f.created, f.regen)
	}

	// secret file lost: the old secret is unrecoverable, so it is rotated
	os.Remove(filepath.Join(dir, "secret"))
	b.mu.Lock()
	b.secretKey = nil
	b.mu.Unlock()
	if err := b.EnsureWebhook(context.Background()); err != nil {
		t.Fatalf("EnsureWebhook (regen): %v", err)
	}
	if f.created != 1 || f.regen != 1 {
		t.Errorf("after lost secret: created=%d regen=%d, want 1/1", f.created, f.regen)
	}
	sec, err = os.ReadFile(filepath.Join(dir, "secret"))
	if err != nil {
		t.Fatalf("secret file not rewritten: %v", err)
	}
	if strings.TrimSpace(string(sec)) != "rotated-s3cret" {
		t.Errorf("stored secret after regen = %q, want rotated-s3cret", strings.TrimSpace(string(sec)))
	}
}

// TestNewRequiresConfig guards the constructor validation.
func TestNewRequiresConfig(t *testing.T) {
	store, _ := agentjob.Open(t.TempDir())
	base := func() Config {
		return Config{
			Plane:      nil,
			Store:      store,
			Projects:   []string{"p"},
			WebhookURL: "http://x",
			SecretFile: t.TempDir() + "/s",
		}
	}
	if _, err := New(base()); err == nil {
		t.Error("New without Plane should fail")
	}
	c := base()
	c.Store = nil
	if _, err := New(c); err == nil {
		t.Error("New without Store should fail")
	}
	c = base()
	c.Projects = nil
	if _, err := New(c); err == nil {
		t.Error("New without Projects should fail")
	}
}

// fixedReadings is a fit.Source that returns the same snapshot every
// time, standing in for nvidia-smi + /proc/meminfo in tests.
type fixedReadings struct{ r fit.Readings }

func (s fixedReadings) Readings(context.Context) (fit.Readings, error) {
	return s.r, nil
}

// writeTestRegistry writes a minimal valid models.yaml with one planner
// model named id at path.
func writeTestRegistry(t *testing.T, path, id string, vramGB, ramGB int) {
	t.Helper()
	reg := fmt.Sprintf(`models:
  - id: %s
    display_name: Test Model
    location: local
    engine: ollama
    endpoint: http://localhost:1/v1
    roles: [planner]
    context_tokens: 1000
    vram_gb: %d
    ram_gb: %d
    thinking: ""
    cost_per_mtok: 0
    enabled: true
    home_location: gpu
    fallback_location: cpu
`, id, vramGB, ramGB)
	if err := os.WriteFile(path, []byte(reg), 0o644); err != nil {
		t.Fatalf("writing test registry: %v", err)
	}
}

// TestPlanningJobQueuedPostsFitComment covers A4 end to end: when a
// planning job is queued, the bridge reads (mocked) live memory, checks
// the planned model's registry requirements, and posts the verdict as a
// comment on the item.
func TestPlanningJobQueuedPostsFitComment(t *testing.T) {
	b, f, store, dir := newBridge(t, "jarvis-bot@x")
	modelsFile := filepath.Join(dir, "models.yaml")
	writeTestRegistry(t, modelsFile, "opus", 20, 80)
	b.cfg.ModelsFile = modelsFile
	b.cfg.FitSource = fixedReadings{fit.Readings{FreeVRAMGB: 10, FreeRAMGB: 100}}

	f.setItem("proj-1", "wi-1", "Ship the widget", "st-backlog", "", 7, "lbl-epic", "lbl-model")
	body := eventBody("wi-1", "proj-1", "nathan@x", "labels")
	if code := postWebhook(t, b, body, sign("s3cret", body)); code != http.StatusOK {
		t.Fatalf("webhook: status %d, want 200", code)
	}
	b.Wait()

	if _, err := store.Get("plane-plan_epic-wi-1"); err != nil {
		t.Fatalf("planning job not queued: %v", err)
	}
	cs := f.Comments()
	if len(cs) != 1 {
		t.Fatalf("comments = %d, want 1: %+v", len(cs), cs)
	}
	if cs[0].project != "proj-1" || cs[0].item != "wi-1" {
		t.Errorf("comment on %s/%s, want proj-1/wi-1", cs[0].project, cs[0].item)
	}
	// 10 GiB free VRAM < 20 needed, 100 GiB free RAM >= 80 needed.
	if !strings.Contains(cs[0].html, "needs_offload") {
		t.Errorf("comment missing needs_offload verdict: %s", cs[0].html)
	}
	if !strings.Contains(cs[0].html, "Test Model") {
		t.Errorf("comment missing the model name: %s", cs[0].html)
	}

	// a redelivered webhook queues nothing and posts no second comment
	if code := postWebhook(t, b, body, sign("s3cret", body)); code != http.StatusOK {
		t.Fatalf("replayed webhook: status %d, want 200", code)
	}
	b.Wait()
	if cs := f.Comments(); len(cs) != 1 {
		t.Errorf("comments after replay = %d, want 1 (idempotent)", len(cs))
	}
}

// TestRunTaskJobNoFitComment: the fit comment goes with planning jobs
// only; a run_task job queues without one.
func TestRunTaskJobNoFitComment(t *testing.T) {
	b, f, store, dir := newBridge(t, "jarvis-bot@x")
	modelsFile := filepath.Join(dir, "models.yaml")
	writeTestRegistry(t, modelsFile, "opus", 20, 80)
	b.cfg.ModelsFile = modelsFile
	b.cfg.FitSource = fixedReadings{fit.Readings{FreeVRAMGB: 10, FreeRAMGB: 100}}

	f.setItem("proj-1", "sub-1", "Sub task", "st-ready", "epic-1", 2)
	body := eventBody("sub-1", "proj-1", "nathan@x", "state")
	if code := postWebhook(t, b, body, sign("s3cret", body)); code != http.StatusOK {
		t.Fatalf("webhook: status %d, want 200", code)
	}
	b.Wait()

	if _, err := store.Get("plane-run_task-sub-1"); err != nil {
		t.Fatalf("run task job not queued: %v", err)
	}
	if cs := f.Comments(); len(cs) != 0 {
		t.Errorf("run task posted %d comments, want 0: %+v", len(cs), cs)
	}
}

// TestFitCommentSkippedOnProblems: a missing models file never blocks or
// fails the queue — the job goes in, the comment is dropped.
func TestFitCommentSkippedOnProblems(t *testing.T) {
	b, f, store, _ := newBridge(t, "jarvis-bot@x")
	b.cfg.ModelsFile = filepath.Join(t.TempDir(), "missing.yaml")
	b.cfg.FitSource = fixedReadings{fit.Readings{FreeVRAMGB: 10, FreeRAMGB: 100}}

	f.setItem("proj-1", "wi-1", "Ship the widget", "st-backlog", "", 7, "lbl-epic", "lbl-model")
	body := eventBody("wi-1", "proj-1", "nathan@x", "labels")
	if code := postWebhook(t, b, body, sign("s3cret", body)); code != http.StatusOK {
		t.Fatalf("webhook: status %d, want 200", code)
	}
	b.Wait()

	if _, err := store.Get("plane-plan_epic-wi-1"); err != nil {
		t.Fatalf("planning job not queued despite missing models file: %v", err)
	}
	if cs := f.Comments(); len(cs) != 0 {
		t.Errorf("missing models file still posted %d comments", len(cs))
	}
}

// TestCatchUpCoversAllWatchedProjects verifies the multi-project behavior:
// one project per epic means the catch-up must sweep every watched
// project, and job titles carry the project's own identifier.
func TestCatchUpCoversAllWatchedProjects(t *testing.T) {
	b, f, store, _ := newBridge(t, "jarvis-bot@x")
	b.cfg.Projects = []string{"proj-1", "proj-2"}
	b.cfg.ProjectNames = map[string]string{"proj-1": "AAB", "proj-2": "T2I"}

	// one trigger item per project, plus a non-trigger item in proj-2
	f.setItem("proj-1", "epic-a", "Epic A", "st-backlog", "", 1, "lbl-epic", "lbl-model")
	f.setItem("proj-2", "epic-b", "Epic B", "st-backlog", "", 1, "lbl-epic", "lbl-model")
	f.setItem("proj-2", "plain", "Plain item", "st-backlog", "", 2)

	n, err := b.CatchUp(context.Background())
	if err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	if n != 2 {
		t.Fatalf("CatchUp queued %d jobs, want 2 (one per project)", n)
	}
	jobA, err := store.Get("plane-plan_epic-epic-a")
	if err != nil {
		t.Fatalf("missing job for proj-1 item: %v", err)
	}
	if !strings.Contains(jobA.Title, "AAB-1") {
		t.Errorf("job title = %q, want AAB-1 in it", jobA.Title)
	}
	jobB, err := store.Get("plane-plan_epic-epic-b")
	if err != nil {
		t.Fatalf("missing job for proj-2 item: %v", err)
	}
	if !strings.Contains(jobB.Title, "T2I-1") {
		t.Errorf("job title = %q, want T2I-1 in it", jobB.Title)
	}
	var instB map[string]any
	if err := json.Unmarshal([]byte(jobB.Instructions), &instB); err != nil {
		t.Fatalf("job instructions are not JSON: %v", err)
	}
	board, _ := instB["board"].(map[string]any)
	if board["project"] != "T2I" {
		t.Errorf("instructions board.project = %v, want T2I", board["project"])
	}
}

// FakePlanQueue is a PlanQueuePublisher for tests: it records what the
// bridge tried to publish and plays back a fixed outcome.
type FakePlanQueue struct {
	mu        sync.Mutex
	Published []queue.Message
	Behind    int
	Err       error
}

func (q *FakePlanQueue) PublishPlan(_ context.Context, m queue.Message) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.Err != nil {
		return 0, q.Err
	}
	q.Published = append(q.Published, m)
	return q.Behind, nil
}

func (q *FakePlanQueue) published() []queue.Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]queue.Message(nil), q.Published...)
}

// TestPlanJobPublishesToFrontOfBackgroundQueue covers B1: the planning
// job is published at the front of the background queue with the plan
// priority, the epic moves to Planning, and the epic gets a queue-
// position comment. A redelivered webhook re-runs none of it.
func TestPlanJobPublishesToFrontOfBackgroundQueue(t *testing.T) {
	b, f, store, _ := newBridge(t, "jarvis-bot@x")
	fq := &FakePlanQueue{Behind: 2}
	b.cfg.PlanQueue = fq

	f.setItem("proj-1", "wi-1", "Ship the widget", "st-backlog", "", 7, "lbl-epic", "lbl-model")
	body := eventBody("wi-1", "proj-1", "nathan@x", "labels")
	if code := postWebhook(t, b, body, sign("s3cret", body)); code != http.StatusOK {
		t.Fatalf("webhook: status %d, want 200", code)
	}
	b.Wait()

	// the store job, with the effort defaulting to high
	job, err := store.Get("plane-plan_epic-wi-1")
	if err != nil {
		t.Fatalf("planning job not queued: %v", err)
	}
	var inst map[string]any
	if err := json.Unmarshal([]byte(job.Instructions), &inst); err != nil {
		t.Fatalf("instructions not JSON: %v", err)
	}
	if inst["model_id"] != "opus" || inst["effort"] != "high" {
		t.Errorf("instructions model_id/effort = %v/%v, want opus/high", inst["model_id"], inst["effort"])
	}

	// one publish, with the shape the worker's ingest path expects
	pubs := fq.published()
	if len(pubs) != 1 {
		t.Fatalf("queue publishes = %d, want 1: %+v", len(pubs), pubs)
	}
	m := pubs[0]
	if m.ID != job.ID || m.Type != queue.BackgroundQueue || m.Priority != queue.PlanEpicPriority {
		t.Errorf("published id/type/priority = %s/%s/%d, want %s/%s/%d",
			m.ID, m.Type, m.Priority, job.ID, queue.BackgroundQueue, queue.PlanEpicPriority)
	}
	if m.Kind != queue.KindAgent || m.Class != agentjob.ClassBackground {
		t.Errorf("published kind/class = %s/%s, want %s/%s", m.Kind, m.Class, queue.KindAgent, agentjob.ClassBackground)
	}
	if m.ReasoningEffort != queue.EffortHigh {
		t.Errorf("published reasoning_effort = %q, want %s", m.ReasoningEffort, queue.EffortHigh)
	}
	if !strings.Contains(m.Title, "plan epic AAB-7") {
		t.Errorf("published title = %q, want it to name the epic", m.Title)
	}

	// the epic moved to Planning
	moves := f.StateMoves()
	if len(moves) != 1 || moves[0].item != "wi-1" || moves[0].state != "st-planning" {
		t.Errorf("state moves = %+v, want wi-1 -> st-planning", moves)
	}

	// the position comment (no models file, so no fit comment alongside)
	cs := f.Comments()
	if len(cs) != 1 {
		t.Fatalf("comments = %d, want 1: %+v", len(cs), cs)
	}
	if !strings.Contains(cs[0].html, "front of the background queue") {
		t.Errorf("position comment missing the queue: %s", cs[0].html)
	}
	if !strings.Contains(cs[0].html, "2 jobs behind") {
		t.Errorf("position comment missing the count: %s", cs[0].html)
	}

	// a redelivered webhook re-runs nothing
	if code := postWebhook(t, b, body, sign("s3cret", body)); code != http.StatusOK {
		t.Fatalf("replayed webhook: status %d, want 200", code)
	}
	b.Wait()
	if n := len(fq.published()); n != 1 {
		t.Errorf("queue publishes after replay = %d, want 1 (idempotent)", n)
	}
	if n := len(f.StateMoves()); n != 1 {
		t.Errorf("state moves after replay = %d, want 1 (idempotent)", n)
	}
	if n := len(f.Comments()); n != 1 {
		t.Errorf("comments after replay = %d, want 1 (idempotent)", n)
	}
}

// TestPlanJobEffortLabel covers B1's effort: label — effort:low rides
// into the job, and an unknown value falls back to the PLAN.md default
// (high) instead of reaching the model.
func TestPlanJobEffortLabel(t *testing.T) {
	b, f, store, _ := newBridge(t, "jarvis-bot@x")
	fq := &FakePlanQueue{}
	b.cfg.PlanQueue = fq
	f.mu.Lock()
	f.labels["lbl-effort-low"] = "effort:low"
	f.labels["lbl-effort-weird"] = "effort:banana"
	f.mu.Unlock()

	f.setItem("proj-1", "wi-low", "Low effort epic", "st-backlog", "", 1, "lbl-epic", "lbl-model", "lbl-effort-low")
	f.setItem("proj-1", "wi-weird", "Weird effort epic", "st-backlog", "", 2, "lbl-epic", "lbl-model", "lbl-effort-weird")
	for _, itemID := range []string{"wi-low", "wi-weird"} {
		body := eventBody(itemID, "proj-1", "nathan@x", "labels")
		if code := postWebhook(t, b, body, sign("s3cret", body)); code != http.StatusOK {
			t.Fatalf("webhook for %s: status %d, want 200", itemID, code)
		}
	}
	b.Wait()

	wantEffort := map[string]string{
		"plane-plan_epic-wi-low":   queue.EffortLow,
		"plane-plan_epic-wi-weird": queue.EffortHigh,
	}
	for id, want := range wantEffort {
		job, err := store.Get(id)
		if err != nil {
			t.Fatalf("missing job %s: %v", id, err)
		}
		var inst map[string]any
		if err := json.Unmarshal([]byte(job.Instructions), &inst); err != nil {
			t.Fatalf("instructions not JSON: %v", err)
		}
		if inst["effort"] != want {
			t.Errorf("%s instructions effort = %v, want %s", id, inst["effort"], want)
		}
	}
	got := map[string]string{}
	for _, m := range fq.published() {
		got[m.ID] = m.ReasoningEffort
	}
	for id, want := range wantEffort {
		if got[id] != want {
			t.Errorf("published %s reasoning_effort = %q, want %s", id, got[id], want)
		}
	}
}

// TestPlanJobPublishFailureFallsBackToStore covers B1's broker-down
// path: the queue publish fails, the job still lands in the store, the
// epic still moves to Planning, and no position comment is posted (the
// job is not actually in the queue).
func TestPlanJobPublishFailureFallsBackToStore(t *testing.T) {
	b, f, store, _ := newBridge(t, "jarvis-bot@x")
	b.cfg.PlanQueue = &FakePlanQueue{Err: errors.New("broker down")}

	f.setItem("proj-1", "wi-1", "Ship the widget", "st-backlog", "", 7, "lbl-epic", "lbl-model")
	body := eventBody("wi-1", "proj-1", "nathan@x", "labels")
	if code := postWebhook(t, b, body, sign("s3cret", body)); code != http.StatusOK {
		t.Fatalf("webhook: status %d, want 200", code)
	}
	b.Wait()

	if _, err := store.Get("plane-plan_epic-wi-1"); err != nil {
		t.Fatalf("planning job missing from store: %v", err)
	}
	if moves := f.StateMoves(); len(moves) != 1 || moves[0].state != "st-planning" {
		t.Errorf("state moves = %+v, want wi-1 -> st-planning", moves)
	}
	if cs := f.Comments(); len(cs) != 0 {
		t.Errorf("failed publish still posted %d comments, want 0", len(cs))
	}
}

// TestInstructionsDescription: the plan job's contract carries the epic's
// description as plain text (the B3 plan prompt reads it).
func TestInstructionsDescription(t *testing.T) {
	cfg := Config{BoardURL: "http://plane", Workspace: "jarvis",
		ProjectNames: map[string]string{"p1": "T2I"}}
	item := &plane.WorkItem{ID: "wi-1", Name: "Epic", SequenceID: 3,
		DescriptionHTML: "<p>Build the <b>widget</b> &amp; ship it.</p>"}
	var body map[string]any
	if err := json.Unmarshal([]byte(instructions(TriggerPlan, cfg, "p1", item, "opus", "high", []string{"epic"})), &body); err != nil {
		t.Fatalf("instructions not JSON: %v", err)
	}
	got, _ := body["item"].(map[string]any)
	if got["description"] != "Build the widget & ship it." {
		t.Fatalf("description = %v, want stripped plain text", got["description"])
	}
}

func TestStripHTML(t *testing.T) {
	cases := map[string]string{
		"<p>a &amp; b</p>\n":    "a & b",
		"plain":                 "plain",
		"":                      "",
		"a&nbsp;b":              "a b",
		"<ul><li>one</li></ul>": "one",
	}
	for in, want := range cases {
		if got := stripHTML(in); got != want {
			t.Errorf("stripHTML(%q) = %q, want %q", in, got, want)
		}
	}
}
