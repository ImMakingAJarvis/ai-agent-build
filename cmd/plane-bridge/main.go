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
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/bridge"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
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

	webhookURL := envOr("BRIDGE_WEBHOOK_URL", "")
	if webhookURL == "" {
		ip, err := lanIP()
		if err != nil {
			return fmt.Errorf("auto webhook URL: %w (set BRIDGE_WEBHOOK_URL)", err)
		}
		webhookURL = fmt.Sprintf("http://%s:%s/webhook/plane", ip, portOf(listen))
	}

	br, err := bridge.New(bridge.Config{
		Plane:        client,
		Store:        store,
		Log:          log,
		Projects:     projectIDs,
		ProjectNames: projectNames,
		WebhookURL:   webhookURL,
		SecretFile:   secretFile,
		BotEmail:     botEmail,
		BoardURL:     planeURL,
		Workspace:    workspace,
		EventsFile:   eventsFile,
	})
	if err != nil {
		return err
	}
	defer br.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	go func() {
		for {
			n, err := br.CatchUp(ctx)
			if err != nil {
				log.Warn("catch-up sync failed, retrying", "err", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(60 * time.Second):
				}
				continue
			}
			log.Info("startup catch-up complete", "queued", n)
			return
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
