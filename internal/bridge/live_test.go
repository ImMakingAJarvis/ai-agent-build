package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

// TestLiveWebhookAndCatchUp is the P4 acceptance test against the real
// board: it builds the plane-bridge binary, runs it against a scratch
// project, and verifies
//
//   - the bridge registers its webhook and the secret is stored;
//   - events made by the Jarvis bot are delivered, pass signature
//     verification and are ignored — no event line, no job (acceptance:
//     "a bot edit does not"); the "webhook ignored (bot event)" log
//     line is what proves live delivery, since it is only written after
//     a successful signature check;
//   - an item put in a trigger state while the bridge is stopped is
//     queued by the catch-up sync on the next start (acceptance:
//     "catch-up works").
//
// The "an item edited by hand logs an event" half needs a human account
// that can edit the scratch project. Project membership requires
// workspace membership, and headless member-add is impossible on this
// build (the invite token is never exposed via the API), so that half
// runs only when PLANE_ADMIN_EMAIL already is a workspace member — i.e.
// after its pending invite (kept alive by plane-setup) is accepted in
// the Plane UI — and adds the admin to the project before the edit.
//
// The scratch project and the workspace webhook are removed afterwards.
// Credentials come from the process environment or the daemon env file
// (~/.config/jarvis/agent.env); without them the test skips, so
// `go test ./...` stays green off this machine.

const liveTestPort = "8193"

var (
	liveEnvOnce sync.Once
	liveEnvVals map[string]string
)

func liveEnvValue(key string) string {
	liveEnvOnce.Do(func() {
		path := os.Getenv("PLANE_ENV_FILE")
		if path == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return
			}
			path = filepath.Join(home, ".config", "jarvis", "agent.env")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		vals := map[string]string{}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			vals[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
		liveEnvVals = vals
	})
	if liveEnvVals == nil {
		return ""
	}
	return liveEnvVals[key]
}

func liveGet(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return liveEnvValue(key)
}

// lanTestIP picks this box's LAN IPv4 (not loopback, link-local or docker).
func lanTestIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if (ip[0] == 172 && ip[1]&0x3f == 16) || (ip[0] == 192 && ip[1] == 168) {
				continue
			}
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no LAN IPv4 address found")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// <root>/internal/bridge/live_test.go
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

// waitFor polls fn until it is true or the timeout elapses; details
// (e.g. log tails, evaluated only on timeout) are appended to the
// failure message.
func waitFor(t *testing.T, timeout time.Duration, what string, fn func() bool, details ...func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	msg := fmt.Sprintf("timed out after %s waiting for %s", timeout, what)
	for _, d := range details {
		if s := d(); s != "" {
			msg += "\n" + s
		}
	}
	t.Fatal(msg)
}

// v1Do is a minimal v1-API request for changes the client does not have
// a method for (here: renaming a work item as the admin account, adding
// the admin to the scratch project).
func v1Do(ctx context.Context, base, key, method, path string, body any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func v1Patch(ctx context.Context, base, key, path string, body any) error {
	return v1Do(ctx, base, key, http.MethodPatch, path, body)
}

func v1Post(ctx context.Context, base, key, path string, body any) error {
	return v1Do(ctx, base, key, http.MethodPost, path, body)
}

func TestLiveWebhookAndCatchUp(t *testing.T) {
	planeURL := liveGet("PLANE_URL")
	botKey := liveGet("PLANE_API_KEY")
	adminKey := liveGet("PLANE_ADMIN_API_KEY")
	botEmail := liveGet("PLANE_BOT_EMAIL")
	botPassword := liveGet("PLANE_BOT_PASSWORD")
	adminEmail := liveGet("PLANE_ADMIN_EMAIL")
	if planeURL == "" || botKey == "" || botEmail == "" || botPassword == "" || adminEmail == "" {
		t.Skip("PLANE_URL/PLANE_API_KEY/PLANE_BOT_EMAIL/PLANE_BOT_PASSWORD/PLANE_ADMIN_EMAIL unavailable")
	}
	// Close the probe immediately: a leaked listener would hold the port
	// until GC and make the child bridge's bind fail with "address
	// already in use".
	probe, err := net.Listen("tcp", ":"+liveTestPort)
	if err != nil {
		t.Skipf("port %s unavailable: %v", liveTestPort, err)
	}
	if err := probe.Close(); err != nil {
		t.Skipf("port %s probe close: %v", liveTestPort, err)
	}
	// Serialize with the other live Plane test (internal/plane): they
	// share the bot API key, and the board rate-limits bursts.
	defer plane.AcquireLiveLock(t, 5*time.Minute)()

	lanIP, err := lanTestIP()
	if err != nil {
		t.Skipf("LAN IP: %v", err)
	}
	webhookURL := fmt.Sprintf("http://%s:%s/webhook/plane", lanIP, liveTestPort)
	t.Logf("live test: webhook %s, bot %s, admin %s", webhookURL, botEmail, adminEmail)

	// The bot owns the workspace, so its session can manage webhooks too;
	// the webhook client methods use the session API, hence Email/Password.
	bot, err := plane.New(plane.Config{
		BaseURL: planeURL, APIKey: botKey, Workspace: "jarvis",
		Email: botEmail, Password: botPassword,
	})
	if err != nil {
		t.Fatalf("plane.New(bot): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// The "edited by hand" half needs a human who can edit the scratch
	// project. Project membership requires workspace membership, and
	// headless member-add is impossible on this build, so the half runs
	// only when the admin account already is a workspace member (its
	// pending invite was accepted in the Plane UI).
	adminID := ""
	if adminKey == "" {
		t.Log("PLANE_ADMIN_API_KEY unset: human-edit assertions skipped")
	} else if members, err := bot.ListWorkspaceMembers(ctx); err != nil {
		t.Fatalf("ListWorkspaceMembers: %v", err)
	} else {
		for i := range members {
			if strings.EqualFold(members[i].Email, adminEmail) {
				adminID = members[i].ID
				break
			}
		}
		if adminID == "" {
			t.Logf("%s is not a workspace member: human-edit assertions skipped (accept the pending invite in the Plane UI to enable them)", adminEmail)
		}
	}

	// scratch project; clean up project and webhook no matter what
	ident := "P4" + rand3(t)
	proj, err := bot.CreateProject(ctx, fmt.Sprintf("p4_live_%d", time.Now().Unix()), ident)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Logf("scratch project %s (%s)", proj.Identifier, proj.ID)
	// LIFO: the webhook is removed before the project it watches.
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		ws, err := bot.ListWebhooks(dctx)
		if err != nil {
			t.Errorf("cleanup: list webhooks: %v", err)
			return
		}
		for _, wh := range ws {
			if wh.URL == webhookURL {
				if err := bot.DeleteWebhook(dctx, wh.ID); err != nil {
					t.Errorf("cleanup: delete webhook %s: %v", wh.ID, err)
				}
			}
		}
	})
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		if err := bot.DeleteProject(dctx, proj.ID); err != nil {
			t.Errorf("cleanup: delete scratch project %s: %v", proj.ID, err)
		}
	})

	// remove stale test webhooks from earlier runs (URLs with the test port)
	for _, wh := range mustListWebhooks(t, bot) {
		if strings.Contains(wh.URL, ":"+liveTestPort) {
			t.Logf("removing stale webhook %s (%s)", wh.ID, wh.URL)
			if err := bot.DeleteWebhook(ctx, wh.ID); err != nil {
				t.Fatalf("DeleteWebhook %s: %v", wh.ID, err)
			}
		}
	}

	// build the binary and prepare a private data dir for this run
	work := t.TempDir()
	bin := filepath.Join(work, "plane-bridge")
	build := exec.Command("go", "build", "-o", bin, "./cmd/plane-bridge")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	dataDir := filepath.Join(work, "data")
	secretFile := filepath.Join(work, "secret")
	eventsFile := filepath.Join(work, "events.jsonl")
	// A3: the bridge also watches a models file for plan: labels. Point it
	// at an empty registry so the real planner labels never touch this
	// scratch project (label sync is covered by planlabels' own live test).
	modelsFile := filepath.Join(work, "models.yaml")
	if err := os.WriteFile(modelsFile, []byte("models: []\n"), 0o644); err != nil {
		t.Fatalf("models file: %v", err)
	}
	logFile, err := os.Create(filepath.Join(work, "bridge.log"))
	if err != nil {
		t.Fatalf("log file: %v", err)
	}

	store, err := agentjob.Open(dataDir)
	if err != nil {
		t.Fatalf("agentjob.Open: %v", err)
	}

	runBridge := func() *exec.Cmd {
		cmd := exec.Command(bin)
		cmd.Env = []string{
			"PLANE_URL=" + planeURL,
			"PLANE_API_KEY=" + botKey,
			"PLANE_WORKSPACE=jarvis",
			"PLANE_PROJECTS=" + proj.ID,
			"PLANE_BOT_EMAIL=" + botEmail,
			"PLANE_BOT_PASSWORD=" + botPassword,
			"BRIDGE_LISTEN=:" + liveTestPort,
			"BRIDGE_WEBHOOK_URL=" + webhookURL,
			"BRIDGE_SECRET_FILE=" + secretFile,
			"BRIDGE_EVENTS_FILE=" + eventsFile,
			"BRIDGE_MODELS_FILE=" + modelsFile,
			"JARVIS_DATA_DIR=" + dataDir,
			"HOME=/home/agent",
		}
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting plane-bridge: %v", err)
		}
		return cmd
	}
	stopBridge := func(cmd *exec.Cmd) {
		if cmd == nil || cmd.Process == nil {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	var cmd *exec.Cmd
	t.Cleanup(func() {
		stopBridge(cmd)
		logFile.Close()
	})

	// --- run 1: webhook registration, bot events ignored ---
	cmd = runBridge()

	waitFor(t, 90*time.Second, "webhook registration", func() bool {
		for _, wh := range mustListWebhooks(t, bot) {
			if wh.URL == webhookURL {
				return true
			}
		}
		return false
	}, func() string {
		tail := readTail(logFile)
		state := "alive"
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			state = "dead (" + err.Error() + ")"
		}
		return fmt.Sprintf("bridge process %s; log:\n%s", state, tail)
	})
	if _, err := os.Stat(secretFile); err != nil {
		t.Fatalf("secret file not written: %v", err)
	}
	t.Log("webhook registered and secret stored")
	// Gate on the start catch-up pass finishing: the trigger checks it
	// runs are actor-agnostic, so item 1 must not exist yet or the
	// bot-ignore assertions below would be muddied.
	waitFor(t, 90*time.Second, "first catch-up pass", func() bool {
		return countLogLines(logFile, "catch-up sync done") >= 1
	}, func() string { return readTail(logFile) })

	epic, err := bot.CreateLabel(ctx, proj.ID, "epic", "#111111")
	if err != nil {
		t.Fatalf("CreateLabel epic: %v", err)
	}
	planLbl, err := bot.CreateLabel(ctx, proj.ID, "plan:test-model", "#222222")
	if err != nil {
		t.Fatalf("CreateLabel plan:test-model: %v", err)
	}

	// item 1: created by the BOT in a trigger state (epic + plan label,
	// Backlog). Its webhooks must be ignored — no event line, no job.
	item1, err := bot.CreateWorkItem(ctx, proj.ID, plane.WorkItemCreate{Name: "p4 live: epic (bot-made)"})
	if err != nil {
		t.Fatalf("CreateWorkItem 1: %v", err)
	}
	if _, err := bot.AddLabels(ctx, proj.ID, item1.ID, epic.ID, planLbl.ID); err != nil {
		t.Fatalf("AddLabels 1: %v", err)
	}
	// The bridge logs "webhook ignored (bot event)" for each bot event
	// it receives, only after a successful signature check — so the
	// line is the live proof that delivery and verification worked.
	waitFor(t, 90*time.Second, "bot events to be delivered and ignored", func() bool {
		return countLogLines(logFile, "webhook ignored (bot event)") >= 1
	}, func() string { return readTail(logFile) })
	time.Sleep(10 * time.Second) // settle: let late bot events land
	if n := countEventItems(t, eventsFile, item1.ID); n != 0 {
		t.Errorf("bot events for item 1 were logged (%d lines), want 0", n)
	}
	if _, err := store.Get("plane-plan_epic-" + item1.ID); err == nil {
		t.Errorf("a job exists for bot-made item 1; bot events must not queue jobs")
	}

	if adminID != "" {
		// item 1, edited by the ADMIN (a human, as far as the bridge is
		// concerned): must be logged and must queue the plan job.
		// Project membership is per-project, so add the admin to the
		// scratch project first — as editor (role 20). A plain add grants
		// the default viewer role (5) on this Plane build, whose members
		// cannot edit work items (the rename below would 403).
		if err := v1Post(ctx, planeURL, botKey, fmt.Sprintf("/api/v1/workspaces/jarvis/projects/%s/members/", proj.ID), map[string]any{"member": adminID, "role": 20}); err != nil {
			t.Fatalf("add admin to project: %v", err)
		}
		patchPath := fmt.Sprintf("/api/v1/workspaces/jarvis/projects/%s/work-items/%s/", proj.ID, item1.ID)
		if err := v1Patch(ctx, planeURL, adminKey, patchPath, map[string]string{"name": "p4 live: epic (human edit)"}); err != nil {
			t.Fatalf("admin rename: %v", err)
		}
		waitFor(t, 90*time.Second, "human edit to be logged and queued", func() bool {
			if countEventItems(t, eventsFile, item1.ID) == 0 {
				return false
			}
			_, err := store.Get("plane-plan_epic-" + item1.ID)
			return err == nil
		}, func() string { return readTail(logFile) })
		job, err := store.Get("plane-plan_epic-" + item1.ID)
		if err != nil {
			t.Fatalf("plan job for human-edited item missing: %v (bridge log below)\n%s", err, readTail(logFile))
		}
		t.Logf("human edit logged and queued: %s", job.Title)
		if actor := eventActor(t, eventsFile, item1.ID); actor != adminEmail {
			t.Errorf("event actor = %q, want %q", actor, adminEmail)
		}
		// settle: the bot events predate the admin event in Plane's
		// webhook queue, so by now they have been processed too — and
		// must not be in the log.
		time.Sleep(10 * time.Second)
		if n := countEventItems(t, eventsFile, item1.ID); n != 1 {
			t.Errorf("event lines for item 1 = %d, want 1 (only the human edit; bot events must be ignored)", n)
		}
	}

	// --- stop, make a trigger change while stopped, restart ---
	stopBridge(cmd)
	cmd = nil
	waitFor(t, 30*time.Second, "bridge to stop", func() bool {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+liveTestPort, 500*time.Millisecond)
		if err != nil {
			return true
		}
		conn.Close()
		return false
	})

	item2, err := bot.CreateWorkItem(ctx, proj.ID, plane.WorkItemCreate{Name: "p4 live: epic (made while stopped)"})
	if err != nil {
		t.Fatalf("CreateWorkItem 2: %v", err)
	}
	if _, err := bot.AddLabels(ctx, proj.ID, item2.ID, epic.ID, planLbl.ID); err != nil {
		t.Fatalf("AddLabels 2: %v", err)
	}
	t.Log("item 2 put in a trigger state while the bridge is stopped")

	cmd = runBridge()
	waitFor(t, 120*time.Second, "catch-up to queue item 2", func() bool {
		_, err := store.Get("plane-plan_epic-" + item2.ID)
		return err == nil
	}, func() string {
		return "bridge log tail:\n" + readTail(logFile)
	})
	if _, err := store.Get("plane-plan_epic-" + item2.ID); err != nil {
		t.Fatalf("catch-up did not queue the stopped-window item: %v (bridge log below)\n%s", err, readTail(logFile))
	}
	t.Log("catch-up on restart queued the item changed while stopped")

	if adminID != "" {
		// the human edit must still be exactly one logged event (no
		// duplicates from redeliveries or the second catch-up)
		if n := countEventItems(t, eventsFile, item1.ID); n != 1 {
			t.Errorf("event lines for item 1 = %d, want 1", n)
		}
	}
}

func rand3(t *testing.T) string {
	t.Helper()
	const alpha = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, 3)
	for i := range b {
		b[i] = alpha[rand.Intn(len(alpha))]
	}
	return string(b)
}

func mustListWebhooks(t *testing.T, c *plane.Client) []plane.Webhook {
	t.Helper()
	ws, err := c.ListWebhooks(context.Background())
	if err != nil {
		t.Fatalf("ListWebhooks: %v", err)
	}
	return ws
}

// countEventItems counts event-log lines about itemID.
func countEventItems(t *testing.T, eventsFile, itemID string) int {
	t.Helper()
	raw, err := os.ReadFile(eventsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("reading events file: %v", err)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil {
			if ev["item_id"] == itemID {
				n++
			}
		}
	}
	return n
}

func eventActor(t *testing.T, eventsFile, itemID string) string {
	t.Helper()
	raw, err := os.ReadFile(eventsFile)
	if err != nil {
		t.Fatalf("reading events file: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil && ev["item_id"] == itemID {
			return fmt.Sprint(ev["actor"])
		}
	}
	return ""
}

func readTail(f *os.File) string {
	data, _ := os.ReadFile(f.Name())
	if len(data) > 4000 {
		data = data[len(data)-4000:]
	}
	return string(data)
}

// countLogLines counts occurrences of needle in the (appended) log file.
func countLogLines(f *os.File, needle string) int {
	data, _ := os.ReadFile(f.Name())
	return strings.Count(string(data), needle)
}
