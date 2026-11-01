package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
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
	mu       sync.Mutex
	labels   map[string]string // id -> name
	states   map[string]string // id -> name
	items    map[string]map[string]any
	whID     string
	whURL    string
	whSecret string
	created  int // webhook creations
	regen    int // secret regenerations
}

func newFakePlane() *fakePlane {
	return &fakePlane{
		labels: map[string]string{"lbl-epic": "epic", "lbl-model": "plan:opus"},
		states: map[string]string{"st-backlog": "Backlog", "st-ready": "Ready"},
		items:  map[string]map[string]any{},
	}
}

// sessionCookie reads the session cookie off the request.
func (f *fakePlane) sessionCookie(r *http.Request) string {
	if c, err := r.Cookie("session-id"); err == nil {
		return c.Value
	}
	return ""
}

// setItem stores a work item with the given state id and label ids.
func (f *fakePlane) setItem(id, name, stateID, parent string, seq int, labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[id] = map[string]any{
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
	_, rest2, _ := strings.Cut(strings.TrimPrefix(rest, "projects/"), "/")
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
		id := strings.Trim(strings.TrimPrefix(rest2, "work-items/"), "/")
		if id == "" {
			all := make([]map[string]any, 0, len(f.items))
			for _, it := range f.items {
				all = append(all, it)
			}
			json.NewEncoder(w).Encode(all)
			return
		}
		it, ok := f.items[id]
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
		Plane:       c,
		Store:       store,
		ProjectID:   "proj-1",
		ProjectName: "AAB",
		WebhookURL:  "http://bridge:8102/webhook/plane",
		SecretFile:  filepath.Join(dir, "secret"),
		BotEmail:    botEmail,
		BoardURL:    srv.URL,
		Workspace:   "testws",
		EventsFile:  filepath.Join(dir, "events.jsonl"),
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
	f.setItem("wi-1", "epic", "st-backlog", "", 1, "lbl-epic", "lbl-model")

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
	f.setItem("wi-1", "Ship the widget", "st-backlog", "", 7, "lbl-epic", "lbl-model")

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
	f.setItem("wi-9", "epic", "st-backlog", "", 1, "lbl-epic", "lbl-model")

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
	f.setItem("epic-1", "Big epic", "st-backlog", "", 1, "lbl-epic", "lbl-model")
	f.setItem("sub-1", "Sub task", "st-ready", "epic-1", 2)
	// not in trigger states:
	f.setItem("plain-1", "Plain backlog item", "st-backlog", "", 3)                   // no labels
	f.setItem("noplan-1", "Epic without plan label", "st-backlog", "", 4, "lbl-epic") // no plan:<model>
	f.setItem("orphan-1", "Ready but no parent", "st-ready", "", 5)                   // not a sub-item

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
			ProjectID:  "p",
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
	c.ProjectID = ""
	if _, err := New(c); err == nil {
		t.Error("New without ProjectID should fail")
	}
}
