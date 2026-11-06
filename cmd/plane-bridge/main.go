// plane-bridge receives Plane webhooks and turns board changes into agent
// jobs (P4 of docs/work-board/PLAN.md). It runs as a systemd service
// (systemd/plane-bridge.service) and, on every start, runs a catch-up
// sync over all watched projects so events missed while it was down are
// picked up. The board standard is one Plane project per epic, so the
// watched set usually grows as epics are added (plane-epic keeps
// PLANE_PROJECTS in sync).
//
// Environment (from ~/.config/jarvis/agent.env):
//
//	PLANE_URL        Plane base URL (required)
//	PLANE_API_KEY    v1 API key (required; the Jarvis bot key — the bot
//	                 owns the workspace, so it may manage webhooks too)
//	PLANE_WORKSPACE  workspace slug (default "jarvis")
//	PLANE_PROJECTS   comma-separated project ids or identifiers to watch
//	                 (default: $PLANE_PROJECT, "AAB")
//	PLANE_PROJECT    single-project fallback for PLANE_PROJECTS
//	PLANE_BOT_EMAIL  bot account whose events are ignored
//	PLANE_BOT_PASSWORD bot session password (required — webhook management
//	                 lives on the session API in this Plane build, so the
//	                 bridge signs in as the bot to register its webhook)
//	BRIDGE_LISTEN    listen address (default ":8102")
//	BRIDGE_WEBHOOK_URL   full webhook URL (default: http://<lan-ip>:<port>/webhook/plane)
//	BRIDGE_SECRET_FILE   webhook HMAC secret file (default ~/.config/jarvis/plane-webhook-secret)
//	BRIDGE_EVENTS_FILE   JSONL event log (default ~/.local/share/jarvis/plane-bridge/events.jsonl)
//	BRIDGE_MODELS_FILE   model registry to keep plan: labels in sync with
//	                 (default /home/agent/ai-agent-build/config/models.yaml)
//	BRIDGE_CATCHUP_INTERVAL  periodic catch-up period (default 5m; 0
//	                 disables it — webhooks only)
//	JARVIS_DATA_DIR      agent job store dir (default ~/.local/share/jarvis)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/bridge"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/placement"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/planlabels"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/registry"
)

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// watchProjectRefs returns the project refs to watch: the comma-separated
// PLANE_PROJECTS list, falling back to the single-project PLANE_PROJECT
// (default "AAB").
func watchProjectRefs() []string {
	raw := strings.TrimSpace(os.Getenv("PLANE_PROJECTS"))
	if raw == "" {
		raw = envOr("PLANE_PROJECT", "AAB")
	}
	var refs []string
	for _, part := range strings.Split(raw, ",") {
		if ref := strings.TrimSpace(part); ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("service", "plane-bridge")
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	planeURL := strings.TrimSpace(os.Getenv("PLANE_URL"))
	apiKey := strings.TrimSpace(os.Getenv("PLANE_API_KEY"))
	if planeURL == "" || apiKey == "" {
		return errors.New("PLANE_URL and PLANE_API_KEY must be set")
	}
	workspace := envOr("PLANE_WORKSPACE", "jarvis")
	projectRefs := watchProjectRefs()
	botEmail := strings.TrimSpace(os.Getenv("PLANE_BOT_EMAIL"))
	botPassword := strings.TrimSpace(os.Getenv("PLANE_BOT_PASSWORD"))
	listen := envOr("BRIDGE_LISTEN", ":8102")
	secretFile := envOr("BRIDGE_SECRET_FILE", defaultSecretFile())
	eventsFile := envOr("BRIDGE_EVENTS_FILE", defaultEventsFile())
	modelsFile := envOr("BRIDGE_MODELS_FILE", "/home/agent/ai-agent-build/config/models.yaml")

	client, err := plane.New(plane.Config{
		BaseURL: planeURL, APIKey: apiKey, Workspace: workspace,
		Email: botEmail, Password: botPassword,
	})
	if err != nil {
		return err
	}

	// Resolve each watched project to an id (identifiers accepted).
	var projectIDs []string
	projectNames := map[string]string{}
	var identifiers []string
	for _, ref := range projectRefs {
		id, name, err := resolveProject(client, workspace, ref)
		if err != nil {
			return fmt.Errorf("resolving project %q: %w", ref, err)
		}
		projectIDs = append(projectIDs, id)
		projectNames[id] = name
		identifiers = append(identifiers, name)
	}
	log.Info("watching projects", "projects", identifiers, "board", planeURL)

	store, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		return fmt.Errorf("opening job store: %w", err)
	}

	// B1: plan_epic jobs jump to the front of the background queue. A
	// RabbitMQ outage must not stop the bridge — the store ingest is the
	// idempotency anchor — so a failed connect degrades to store-only.
	// The relay makes that degradation temporary: a connect retry until
	// the broker is back, then a watchdog that reconnects a dead
	// connection (broker restart, network drop), so the C1 catch-up
	// re-trigger of run jobs can publish them without a bridge restart.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	relay := &queueRelay{}
	if qconn, err := queue.NewConn(); err != nil {
		log.Warn("rabbitmq unavailable; jobs are store-only until the broker is back", "err", err)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(queueRetry):
				}
				qc, err := queue.NewConn()
				if err != nil {
					log.Warn("rabbitmq still unavailable, retrying", "err", err)
					continue
				}
				relay.set(queue.NewPlanPublisher(qc, queue.BackgroundQueue), queue.NewLocationPublisher(qc))
				log.Info("queue publishers ready (broker back)")
				go watchConn(ctx, log, qc)
				return
			}
		}()
	} else {
		defer qconn.Close()
		relay.set(queue.NewPlanPublisher(qconn, queue.BackgroundQueue), queue.NewLocationPublisher(qconn))
		log.Info("plan queue publisher ready", "queue", queue.BackgroundQueue)
		// C1: run_task jobs are placement-routed to the location queues
		// (jobs.gpu/cpu/igpu), consumed by the host task-runner.
		log.Info("location queue publisher ready")
		go watchConn(ctx, log, qconn)
	}

	webhookURL := envOr("BRIDGE_WEBHOOK_URL", "")
	if webhookURL == "" {
		ip, err := lanIP()
		if err != nil {
			return fmt.Errorf("auto webhook URL: %w (set BRIDGE_WEBHOOK_URL)", err)
		}
		webhookURL = fmt.Sprintf("http://%s:%s/webhook/plane", ip, portOf(listen))
	}

	// C1: the run_task placement decision needs to know which locations
	// this box has (A5); detect once and share with the board-label
	// watcher below.
	present := placement.Detect(ctx)
	log.Info("box locations detected", "locations", placement.Names(present))

	br, err := bridge.New(bridge.Config{
		Plane:         client,
		Store:         store,
		Log:           log,
		Projects:      projectIDs,
		ProjectNames:  projectNames,
		WebhookURL:    webhookURL,
		SecretFile:    secretFile,
		BotEmail:      botEmail,
		BoardURL:      planeURL,
		Workspace:     workspace,
		EventsFile:    eventsFile,
		ModelsFile:    modelsFile,
		PlanQueue:     relay,
		LocationQueue: relay,
		Present:       present,
	})
	if err != nil {
		return err
	}
	defer br.Close()

	// Webhook registration, retried until Plane is reachable.
	go func() {
		for {
			if err := br.EnsureWebhook(ctx); err != nil {
				log.Warn("webhook registration failed, retrying", "err", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(30 * time.Second):
				}
				continue
			}
			return
		}
	}()

	// Catch-up sync on every start; retried until the first success so a
	// Plane outage at boot does not lose the catch-up.
	catchUpDone := make(chan struct{})
	go func() {
		for {
			// A restart reaps claimed/running entries whose owner PID is
			// gone (crashed task-runner, reboot) so the catch-up below
			// can re-trigger them.
			if err := store.Recover(); err != nil {
				log.Warn("store recover failed", "err", err)
			}
			n, err := br.CatchUp(ctx)
			if err != nil {
				log.Warn("catch-up sync failed, retrying", "err", err)
				select {
				case <-ctx.Done():
					close(catchUpDone)
					return
				case <-time.After(60 * time.Second):
				}
				continue
			}
			log.Info("startup catch-up complete", "queued", n)
			close(catchUpDone)
			return
		}
	}()

	// C1: periodic catch-up. A webhook re-runs evaluate only for the
	// item that changed, so a card whose trigger state re-enters
	// silently is only seen here: a dep-waiting run settles its entry
	// skipped while the card stays Ready, and the dep finishing emits no
	// event for it. Recover first — a crashed task-runner leaves its
	// entry claimed with a dead PID, which reads as in flight.
	go func() {
		interval, perr := time.ParseDuration(envOr("BRIDGE_CATCHUP_INTERVAL", "5m"))
		if perr != nil {
			log.Error("bad BRIDGE_CATCHUP_INTERVAL; periodic catch-up disabled", "err", perr)
			return
		}
		if interval == 0 {
			return
		}
		log.Info("periodic catch-up enabled", "interval", interval.String())
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if err := store.Recover(); err != nil {
				log.Warn("store recover failed", "err", err)
				continue
			}
			n, err := br.CatchUp(ctx)
			if err != nil {
				log.Warn("periodic catch-up failed", "err", err)
				continue
			}
			if n > 0 {
				log.Info("periodic catch-up queued jobs", "queued", n)
			}
		}
	}()

	// Keep the board labels in sync with the registry and the box:
	// plan:<model> with the enabled planner models (A3), model:<id> with
	// the enabled executor models and run:<loc> with the locations this
	// box actually has (A5). Runs on every change to the models file
	// (15 s poll); the first sync waits for the startup catch-up so the
	// bursts do not share the Plane rate limit at boot.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-catchUpDone:
		}
		w := planlabels.NewWatcher(modelsFile, 15*time.Second, log,
			func(ctx context.Context, f *registry.File) error {
				if err := planlabels.Sync(ctx, client, projectIDs, f, log); err != nil {
					return err
				}
				return placement.Sync(ctx, client, projectIDs, f, present, log)
			})
		if err := w.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error("board-label watcher stopped", "err", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/plane", br.HandleWebhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	log.Info("plane-bridge listening", "addr", listen, "webhook", webhookURL,
		"secret_file", secretFile, "events_file", eventsFile)

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		br.Wait()
		log.Info("plane-bridge stopped")
		return nil
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	}
}

// resolveProject finds the project by id or identifier and returns its id
// and identifier.
func resolveProject(client *plane.Client, workspace, ref string) (id, name string, err error) {
	projects, err := client.ListProjects(context.Background())
	if err != nil {
		return "", "", err
	}
	for _, p := range projects {
		if p.ID == ref || strings.EqualFold(p.Identifier, ref) {
			return p.ID, p.Identifier, nil
		}
	}
	return "", "", fmt.Errorf("project %q not found in workspace %s", ref, workspace)
}

// portOf extracts the port from a listen address like ":8102" or "1.2.3.4:8102".
func portOf(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "8102"
	}
	return port
}

// lanIP picks the first public-ish IPv4 address of this box: not
// loopback, not link-local, and not a docker bridge range (172.16/12,
// 192.168/16). It is what Plane (on the NAS) posts the webhook to.
func lanIP() (string, error) {
	isDocker := func(ip net.IP) bool {
		b := ip.To4()
		return b != nil && b[0] == 172 && b[1]&0x3f == 16
	}
	var candidates []string
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
			if ip == nil {
				continue
			}
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if isDocker(ip) || (ip[0] == 192 && ip[1] == 168) {
				continue
			}
			candidates = append(candidates, ip.String())
		}
	}
	if len(candidates) == 0 {
		return "", errors.New("no LAN IPv4 address found")
	}
	return candidates[0], nil
}

func defaultSecretFile() string {
	home, _ := os.UserHomeDir()
	return home + "/.config/jarvis/plane-webhook-secret"
}

func defaultEventsFile() string {
	return agentjob.DataDir() + "/plane-bridge/events.jsonl"
}

// queueRetry is the period for the broker connect retry and the dead-
// connection watchdog.
const queueRetry = 30 * time.Second

// queueRelay holds the queue publishers behind a mutex so a (re)connect
// can swap them in without touching a publish in flight. While no
// publisher is set (broker down at start, before the retry lands) the
// methods fail with the error the bridge expects: the job settles failed
// and the catch-up re-triggers it once the publisher is back.
type queueRelay struct {
	mu       sync.Mutex
	plan     bridge.PlanQueuePublisher
	location bridge.LocationQueuePublisher
}

func (r *queueRelay) set(plan bridge.PlanQueuePublisher, location bridge.LocationQueuePublisher) {
	r.mu.Lock()
	r.plan = plan
	r.location = location
	r.mu.Unlock()
}

func (r *queueRelay) PublishPlan(ctx context.Context, m queue.Message) (int, error) {
	r.mu.Lock()
	p := r.plan
	r.mu.Unlock()
	if p == nil {
		return 0, errors.New("no plan queue publisher (broker down at bridge start)")
	}
	return p.PublishPlan(ctx, m)
}

func (r *queueRelay) PublishLocation(ctx context.Context, loc string, m queue.Message) error {
	r.mu.Lock()
	p := r.location
	r.mu.Unlock()
	if p == nil {
		return errors.New("no location queue publisher (broker down at bridge start)")
	}
	return p.PublishLocation(ctx, loc, m)
}

// watchConn reconnects qconn while its underlying AMQP connection is dead
// (broker restart, network drop). Reconnect is safe under concurrent
// publish: a publish racing the swap errors once and the bridge's
// store-settle plus catch-up re-trigger retries it.
func watchConn(ctx context.Context, log *slog.Logger, qconn *queue.Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(queueRetry):
		}
		if !qconn.Dead() {
			continue
		}
		if err := qconn.Reconnect(); err != nil {
			log.Warn("rabbitmq reconnect failed, retrying", "err", err)
			continue
		}
		log.Info("rabbitmq reconnected; publishers live again")
	}
}
