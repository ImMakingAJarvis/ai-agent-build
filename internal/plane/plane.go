// Package plane is a small client for the self-hosted Plane board
// (docs/work-board/PLAN.md). It covers the work-item operations the agent
// needs: create work item and sub-item, set state, add and remove labels,
// add "blocked by" relations, comment, and create a page.
//
// This Plane deployment exposes two API surfaces, verified live in P1-P3:
//
//   - the v1 external API under /api/v1/..., authenticated with the
//     X-Api-Key header. It covers projects, states, labels, work items,
//     relations and comments.
//   - the session API under /api/..., authenticated like a browser session
//     (email/password sign-in, session cookie, CSRF token on mutations).
//     Pages are only exposed here; the API key 401s on these routes.
//
// Endpoint quirks specific to this build (2026 Plane, behaves like 1.4.x)
// are noted on the methods that work around them.
package plane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config configures a Client.
type Config struct {
	// BaseURL is the Plane instance, e.g. "http://111.1.2.222:8088".
	BaseURL string
	// APIKey is the v1 external API key (X-Api-Key header).
	APIKey string
	// Workspace is the workspace slug all calls are scoped to, e.g. "jarvis".
	Workspace string
	// Email/Password are the session credentials used for session-API
	// endpoints (pages). Only required for those calls.
	Email    string
	Password string
	// HTTP is an optional transport; defaults to a 30 s client with a
	// cookie jar (needed for session sign-in).
	HTTP *http.Client
}

// Client talks to one workspace of a Plane instance. It is safe for
// concurrent use.
type Client struct {
	baseURL   string
	apiKey    string
	workspace string
	email     string
	password  string
	http      *http.Client
	login     *http.Client

	mu       sync.Mutex
	csrf     string
	loggedIn bool
}

// New builds a Client from cfg. BaseURL and Workspace are required; APIKey
// is required for every call except the page methods, which need
// Email/Password instead.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("plane: BaseURL is required")
	}
	if cfg.Workspace == "" {
		return nil, errors.New("plane: Workspace is required")
	}
	transport := cfg.HTTP
	if transport == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, err
		}
		transport = &http.Client{Jar: jar, Timeout: 30 * time.Second}
	}
	// A separate client for the sign-in POST: same jar, but it never
	// follows redirects. A successful sign-in 302s to Plane's SITE_URL,
	// which on the NAS deploy points at an IP this box cannot reach; the
	// session cookie in the sign-in response is all we need.
	timeout := transport.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	login := &http.Client{
		Jar:     transport.Jar,
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:    cfg.APIKey,
		workspace: cfg.Workspace,
		email:     cfg.Email,
		password:  cfg.Password,
		http:      transport,
		login:     login,
	}, nil
}

// envValue resolves a Plane setting: the process environment first, then
// the KEY=VALUE daemon env file that scripts/plane-setup.sh writes
// (~/.config/jarvis/agent.env, or the path in PLANE_ENV_FILE). The file is
// small and CLIs are short-lived, so it is re-read on every miss.
func envValue(key string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	path := os.Getenv("PLANE_ENV_FILE")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(home, ".config", "jarvis", "agent.env")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// FromEnv builds a Client from the process environment:
//
//	PLANE_URL       Plane instance, e.g. http://111.1.2.222:8088 (required)
//	PLANE_API_KEY   v1 external API key (required)
//	PLANE_WORKSPACE workspace slug (default "jarvis")
//	PLANE_BOT_EMAIL / PLANE_BOT_PASSWORD  session creds for pages (optional)
//
// Missing values fall back to the daemon env file (see envValue), so CLIs
// run from a plain shell work without exporting anything.
func FromEnv() (*Client, error) {
	ws := envValue("PLANE_WORKSPACE")
	if ws == "" {
		ws = "jarvis"
	}
	return New(Config{
		BaseURL:   envValue("PLANE_URL"),
		APIKey:    envValue("PLANE_API_KEY"),
		Workspace: ws,
		Email:     envValue("PLANE_BOT_EMAIL"),
		Password:  envValue("PLANE_BOT_PASSWORD"),
	})
}

// APIError is a non-2xx reply from Plane. Body is the raw response
// (reads are capped at 1 MB).
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	return fmt.Sprintf("plane: %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, body)
}

// IsStatus reports whether the error is an APIError with the given status.
func (e *APIError) IsStatus(code int) bool { return e.Status == code }

// Rate-limit backoff parameters. Plane on this instance rate-limits
// bursty v1 traffic (HTTP 429 RATE_LIMIT_EXCEEDED) on reads and writes
// alike, so do/doList wait out the window and retry — callers need no
// backoff of their own.
const (
	rateLimitPause  = 30 * time.Second
	maxRateAttempts = 10
)

// isRateLimited reports whether err is a Plane 429 rate-limit response.
func isRateLimited(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.IsStatus(http.StatusTooManyRequests)
}

// waitRateLimit sleeps out one rate-limit window, honouring ctx.
func waitRateLimit(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(rateLimitPause):
		return nil
	}
}

// Project is a Plane project (a board).
type Project struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Identifier string `json:"identifier"` // short code, e.g. "AAB"; work items are AAB-n
	Slug       string `json:"slug"`
}

// State is a work-item state (a column), e.g. Backlog or Done.
type State struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Group    string  `json:"group"` // backlog, unstarted, started, completed, cancelled
	Sequence float64 `json:"sequence"`
	Color    string  `json:"color"`
}

// Label is a project label, e.g. "epic".
type Label struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// WorkItem is a work item (issue). Field names follow this build's
// serializer: the state is "state" (not "state_id") and the parent is
// "parent" (not "parent_id"). Labels holds label IDs.
type WorkItem struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	DescriptionHTML string   `json:"description_html"`
	State           string   `json:"state"` // state ID
	Labels          []string `json:"labels"`
	Parent          string   `json:"parent"` // parent work item ID, "" if none
	SequenceID      int      `json:"sequence_id"`
	CreatedAt       string   `json:"created_at"`
	// Priority is one of "urgent", "high", "medium", "low", "none"
	// ("none" when unset).
	Priority string `json:"priority"`
	// Assignees holds user IDs (workspace members of the item's project).
	Assignees []string `json:"assignees"`
}

// WorkItemCreate is the payload for CreateWorkItem. Parent, when set,
// makes the item a sub-item of the parent. Assignees must be project
// members.
type WorkItemCreate struct {
	Name            string   `json:"name"`
	DescriptionHTML string   `json:"description_html,omitempty"`
	Parent          string   `json:"parent,omitempty"`
	State           string   `json:"state,omitempty"`
	Labels          []string `json:"labels,omitempty"`
	Priority        string   `json:"priority,omitempty"`
	Assignees       []string `json:"assignees,omitempty"`
}

// RelationRef names one end of a relation as returned by the relations
// endpoint.
type RelationRef struct {
	ProjectID string `json:"project_id"`
	IssueID   string `json:"issue_id"`
}

// Relations is the full relation set of a work item, grouped by type.
type Relations struct {
	Blocking     []RelationRef `json:"blocking"`
	BlockedBy    []RelationRef `json:"blocked_by"`
	Duplicate    []RelationRef `json:"duplicate"`
	RelatesTo    []RelationRef `json:"relates_to"`
	StartAfter   []RelationRef `json:"start_after"`
	StartBefore  []RelationRef `json:"start_before"`
	FinishAfter  []RelationRef `json:"finish_after"`
	FinishBefore []RelationRef `json:"finish_before"`
}

// Comment is a work-item comment.
type Comment struct {
	ID          string `json:"id"`
	CommentHTML string `json:"comment_html"`
	CreatedAt   string `json:"created_at"`
}

// Page is a project page (a Plane doc).
type Page struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentHTML string `json:"content_html"`
	CreatedAt   string `json:"created_at"`
}

// PageCreate is the payload for CreatePage.
type PageCreate struct {
	Name        string `json:"name"`
	ContentHTML string `json:"content_html,omitempty"`
}

// --- projects -----------------------------------------------------------------

// CreateProject creates a project in the workspace. Name may not contain
// hyphens (this build rejects them); Identifier is the short code.
func (c *Client) CreateProject(ctx context.Context, name, identifier string) (*Project, error) {
	var p Project
	body := map[string]string{"name": name, "identifier": identifier}
	if err := c.do(ctx, "POST", c.wsPath("/projects/"), body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListProjects lists the workspace's projects (first page; the workspace
// has a handful of them).
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	var out []Project
	if err := c.doList(ctx, "GET", c.wsPath("/projects/"), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteProject deletes a project and everything in it.
func (c *Client) DeleteProject(ctx context.Context, projectID string) error {
	return c.do(ctx, "DELETE", c.wsPath("/projects/")+projectID+"/", nil, nil)
}

// GetProject fetches one project by UUID.
func (c *Client) GetProject(ctx context.Context, projectID string) (*Project, error) {
	var p Project
	if err := c.do(ctx, "GET", c.wsPath("/projects/"+projectID+"/"), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ProjectByRef resolves a project by UUID or by identifier (e.g. "AAB").
func ProjectByRef(ctx context.Context, c *Client, ref string) (*Project, error) {
	if isUUID(ref) {
		return c.GetProject(ctx, ref)
	}
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	for i := range projects {
		if projects[i].Identifier == ref || projects[i].ID == ref {
			return &projects[i], nil
		}
	}
	return nil, fmt.Errorf("plane: no project %q in workspace %s", ref, c.workspace)
}

// ResolveItemRef resolves an item reference to (project, item ref) for
// the one-project-per-epic board. A prefixed display id ("T2I-3") selects
// the project by its identifier prefix (case-insensitive), so one ref can
// address any project without --project. UUIDs and bare sequences use
// projectRef instead. A prefix that matches no project falls through to
// projectRef, which then fails with the usual "no work item" error.
func ResolveItemRef(ctx context.Context, c *Client, projectRef, itemRef string) (*Project, string, error) {
	if !isUUID(itemRef) {
		if prefix, _, ok := strings.Cut(itemRef, "-"); ok && prefix != "" {
			projects, err := c.ListProjects(ctx)
			if err != nil {
				return nil, "", err
			}
			for i := range projects {
				if strings.EqualFold(projects[i].Identifier, prefix) {
					return &projects[i], itemRef, nil
				}
			}
		}
	}
	p, err := ProjectByRef(ctx, c, projectRef)
	if err != nil {
		return nil, "", err
	}
	return p, itemRef, nil
}

// isUUID reports whether s looks like a Plane UUID (8-4-4-4-12 hex).
func isUUID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// WorkspaceMember is one entry of the v1 workspace member list.
type WorkspaceMember struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Role  int    `json:"role"`
}

// ListWorkspaceMembers lists the workspace members (v1). The live test
// uses it to detect when the human admin account has joined the
// workspace (its pending invite was accepted in the Plane UI).
func (c *Client) ListWorkspaceMembers(ctx context.Context) ([]WorkspaceMember, error) {
	var out []WorkspaceMember
	if err := c.doList(ctx, "GET", c.wsPath("/members/"), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ProjectMember is one entry of the v1 project member list (a user; the
// project role is not in the response).
type ProjectMember struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// ListProjectMembers lists a project's members (v1, bare array).
func (c *Client) ListProjectMembers(ctx context.Context, projectID string) ([]ProjectMember, error) {
	var out []ProjectMember
	if err := c.doList(ctx, "GET", c.projectPath(projectID, "/members/"), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AddProjectMember adds a workspace member to a project. Role is the Plane
// project role code (5 viewer, 20 editor, 30 admin) — a bare add grants
// the default viewer role on this build, whose members cannot edit work
// items, so pass the role explicitly. Adding a current member is a 400.
func (c *Client) AddProjectMember(ctx context.Context, projectID, memberID string, role int) error {
	body := map[string]any{"member": memberID, "role": role}
	return c.do(ctx, "POST", c.projectPath(projectID, "/members/"), body, nil)
}

// --- states -------------------------------------------------------------------

// ListStates lists a project's states, in sequence order (first page).
func (c *Client) ListStates(ctx context.Context, projectID string) ([]State, error) {
	var out []State
	if err := c.doList(ctx, "GET", c.projectPath(projectID, "/states/"), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// StateByName finds a project state by name, case-insensitively (this
// build matches state names case-insensitively too).
func (c *Client) StateByName(ctx context.Context, projectID, name string) (*State, error) {
	states, err := c.ListStates(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range states {
		if strings.EqualFold(states[i].Name, name) {
			return &states[i], nil
		}
	}
	return nil, fmt.Errorf("plane: state %q not found in project %s", name, projectID)
}

// --- labels -------------------------------------------------------------------

// ListLabels lists a project's labels (first page).
func (c *Client) ListLabels(ctx context.Context, projectID string) ([]Label, error) {
	var out []Label
	if err := c.doList(ctx, "GET", c.projectPath(projectID, "/labels/"), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateLabel creates a label in a project.
func (c *Client) CreateLabel(ctx context.Context, projectID, name, color string) (*Label, error) {
	var l Label
	body := map[string]string{"name": name}
	if color != "" {
		body["color"] = color
	}
	if err := c.do(ctx, "POST", c.projectPath(projectID, "/labels/"), body, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// LabelByName finds a project label by name, case-insensitively.
func (c *Client) LabelByName(ctx context.Context, projectID, name string) (*Label, error) {
	labels, err := c.ListLabels(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range labels {
		if strings.EqualFold(labels[i].Name, name) {
			return &labels[i], nil
		}
	}
	return nil, fmt.Errorf("plane: label %q not found in project %s", name, projectID)
}

// DeleteLabel removes a project label (by label ID). Work items that
// carry the label lose it — Plane drops the association with the label.
func (c *Client) DeleteLabel(ctx context.Context, projectID, labelID string) error {
	return c.do(ctx, "DELETE", c.projectPath(projectID, "/labels/"+labelID+"/"), nil, nil)
}

// --- work items ----------------------------------------------------------------

// CreateWorkItem creates a work item; with WorkItemCreate.Parent set it
// creates a sub-item under the parent.
func (c *Client) CreateWorkItem(ctx context.Context, projectID string, wi WorkItemCreate) (*WorkItem, error) {
	var out WorkItem
	if err := c.do(ctx, "POST", c.projectPath(projectID, "/work-items/"), wi, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetWorkItem fetches one work item.
func (c *Client) GetWorkItem(ctx context.Context, projectID, itemID string) (*WorkItem, error) {
	var out WorkItem
	if err := c.do(ctx, "GET", c.itemPath(projectID, itemID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteWorkItem hard-deletes a work item. This build has no archive
// (PATCH archived_at is a 400, the session PATCH route 404s), so the
// delete is immediate and irreversible — callers should keep a record of
// what they delete (the project-split migration records one in the
// recreated item's split-from marker).
func (c *Client) DeleteWorkItem(ctx context.Context, projectID, itemID string) error {
	return c.do(ctx, "DELETE", c.itemPath(projectID, itemID), nil, nil)
}

// WorkItemByRef resolves a work item by UUID, by display id like "AAB-43",
// or by bare sequence number "43". This build's v1 API only takes a UUID
// directly (a display id 404s), so the other forms fall back to a list and
// match; the prefix, when given, is checked against the project identifier.
func WorkItemByRef(ctx context.Context, c *Client, projectID, ref string) (*WorkItem, error) {
	if isUUID(ref) {
		return c.GetWorkItem(ctx, projectID, ref)
	}
	var prefix, seqPart string
	if i := strings.LastIndex(ref, "-"); i >= 0 {
		prefix, seqPart = ref[:i], ref[i+1:]
	} else {
		seqPart = ref
	}
	seq, err := strconv.Atoi(seqPart)
	if err != nil {
		return nil, fmt.Errorf("plane: %q is neither a UUID nor a display id like %q", ref, "AAB-43")
	}
	var ident string
	if prefix != "" {
		p, err := c.GetProject(ctx, projectID)
		if err != nil {
			return nil, err
		}
		ident = p.Identifier
	}
	items, err := c.ListWorkItems(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].SequenceID == seq && (prefix == "" || strings.EqualFold(prefix, ident)) {
			return &items[i], nil
		}
	}
	return nil, fmt.Errorf("plane: no work item %q in project %s", ref, projectID)
}

// ItemLink returns the browser URL of a work item on this instance. The
// 2026 Plane UI resolves /<workspace>/browse/<identifier>-<sequence>; the
// older /projects/.../issues/... URLs redirect there, so browse is the
// canonical shareable form.
func (c *Client) ItemLink(p *Project, wi *WorkItem) string {
	return fmt.Sprintf("%s/%s/browse/%s-%d", c.baseURL, c.workspace, p.Identifier, wi.SequenceID)
}

// SetState moves a work item to the given state (by state ID).
func (c *Client) SetState(ctx context.Context, projectID, itemID, stateID string) (*WorkItem, error) {
	var out WorkItem
	body := map[string]string{"state": stateID}
	if err := c.do(ctx, "PATCH", c.itemPath(projectID, itemID), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AddLabels attaches labels to a work item (by label ID). It fetches the
// item first because this build's PATCH replaces the whole label list
// rather than merging.
func (c *Client) AddLabels(ctx context.Context, projectID, itemID string, labelIDs ...string) (*WorkItem, error) {
	cur, err := c.GetWorkItem(ctx, projectID, itemID)
	if err != nil {
		return nil, err
	}
	var out WorkItem
	body := map[string][]string{"labels": unionStrings(cur.Labels, labelIDs)}
	if err := c.do(ctx, "PATCH", c.itemPath(projectID, itemID), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveLabels detaches labels from a work item (by label ID), keeping any
// others. Like AddLabels it reads the item first, because PATCH replaces
// the whole label list.
func (c *Client) RemoveLabels(ctx context.Context, projectID, itemID string, labelIDs ...string) (*WorkItem, error) {
	cur, err := c.GetWorkItem(ctx, projectID, itemID)
	if err != nil {
		return nil, err
	}
	var out WorkItem
	body := map[string][]string{"labels": minusStrings(cur.Labels, labelIDs)}
	if err := c.do(ctx, "PATCH", c.itemPath(projectID, itemID), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- relations ------------------------------------------------------------------

// AddBlockedBy records "blocked by" relations on a work item: the item is
// blocked by each of the given work items.
func (c *Client) AddBlockedBy(ctx context.Context, projectID, itemID string, blockedByIDs ...string) error {
	// The relation payload field is "issues" (a list of work item IDs) —
	// singular field names are rejected with "This field is required".
	body := map[string]any{"relation_type": "blocked_by", "issues": blockedByIDs}
	return c.do(ctx, "POST", c.itemPath(projectID, itemID)+"relations/", body, nil)
}

// Relations returns the full relation set of a work item.
func (c *Client) Relations(ctx context.Context, projectID, itemID string) (*Relations, error) {
	var r Relations
	if err := c.do(ctx, "GET", c.itemPath(projectID, itemID)+"relations/", nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// --- comments -------------------------------------------------------------------

// AddComment posts a comment on a work item. html is the comment body as
// HTML (this build's input field is "comment_html"; "comment_text" is
// silently ignored).
func (c *Client) AddComment(ctx context.Context, projectID, itemID, html string) (*Comment, error) {
	var cm Comment
	body := map[string]string{"comment_html": html}
	if err := c.do(ctx, "POST", c.itemPath(projectID, itemID)+"comments/", body, &cm); err != nil {
		return nil, err
	}
	return &cm, nil
}

// ListComments lists a work item's comments, oldest first.
func (c *Client) ListComments(ctx context.Context, projectID, itemID string) ([]Comment, error) {
	var out []Comment
	if err := c.doList(ctx, "GET", c.itemPath(projectID, itemID)+"comments/", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// --- pages (session API) ----------------------------------------------------------

// CreatePage creates a page in a project. Pages are only exposed on the
// session API, so this uses the Email/Password session, not the API key.
func (c *Client) CreatePage(ctx context.Context, projectID string, pg PageCreate) (*Page, error) {
	if c.email == "" || c.password == "" {
		return nil, errors.New("plane: pages need session credentials (Email/Password)")
	}
	var out Page
	path := c.sessionPath("/workspaces/%s/projects/%s/pages/", c.workspace, projectID)
	if err := c.doSession(ctx, "POST", path, pg, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPages lists a project's pages (session API, first page).
func (c *Client) ListPages(ctx context.Context, projectID string) ([]Page, error) {
	if c.email == "" || c.password == "" {
		return nil, errors.New("plane: pages need session credentials (Email/Password)")
	}
	var out []Page
	path := c.sessionPath("/workspaces/%s/projects/%s/pages/", c.workspace, projectID)
	if err := c.doSessionList(ctx, "GET", path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// --- work items (list) ------------------------------------------------------------

// ListWorkItems lists a project's work items, following the pagination.
func (c *Client) ListWorkItems(ctx context.Context, projectID string) ([]WorkItem, error) {
	const limit = 100
	var all []WorkItem
	for page := 1; ; page++ {
		q := fmt.Sprintf("?limit=%d&page=%d", limit, page)
		var out []WorkItem
		if err := c.doList(ctx, "GET", c.projectPath(projectID, "/work-items/"+q), &out); err != nil {
			return nil, err
		}
		all = append(all, out...)
		if len(out) < limit {
			break
		}
	}
	return all, nil
}

// --- states (create) ----------------------------------------------------------------

// CreateState creates a work-item state (column) in a project. Sequence is
// a float — this build stores and returns it as one (see P2 notes).
func (c *Client) CreateState(ctx context.Context, projectID, name, group, color string, sequence float64) (*State, error) {
	body := map[string]any{"name": name, "group": group, "sequence": sequence, "description": ""}
	if color != "" {
		body["color"] = color
	}
	var s State
	if err := c.do(ctx, "POST", c.projectPath(projectID, "/states/"), body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// DeleteState removes a work-item state (column) from a project. Only
// safe when no work item references the state (the caller must check —
// the API does not).
func (c *Client) DeleteState(ctx context.Context, projectID, stateID string) error {
	return c.do(ctx, "DELETE", c.projectPath(projectID, "/states/"+stateID+"/"), nil, nil)
}

// --- webhooks -----------------------------------------------------------------------

// Webhook is a workspace webhook. SecretKey (the HMAC signing secret) is
// only returned by the API on create and secret regeneration — never on
// list or retrieve — so callers must persist it when they get it.
type Webhook struct {
	ID           string `json:"id"`
	URL          string `json:"url"`
	IsActive     bool   `json:"is_active"`
	SecretKey    string `json:"secret_key,omitempty"`
	Project      bool   `json:"project"`
	Issue        bool   `json:"issue"`
	IssueComment bool   `json:"issue_comment"`
}

// The webhook endpoints are not on the v1 external API in this Plane
// build — they exist only on the session API under
// /api/workspaces/<slug>/webhooks/ and require a workspace ADMIN. All four
// methods below therefore authenticate with the configured session
// credentials (Email/Password in Config), not the API key.

// ListWebhooks lists the workspace's webhooks (secrets not included).
func (c *Client) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	var out []Webhook
	if err := c.doSessionList(ctx, "GET", c.sessionPath("/workspaces/%s/webhooks/", c.workspace), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateWebhook registers a workspace webhook for the given event types
// ("issue", "issue_comment", "project", "cycle", "module") and returns the
// reply with the generated SecretKey — the only moment the API reveals it
// (besides RegenerateWebhookSecret).
func (c *Client) CreateWebhook(ctx context.Context, url string, eventTypes ...string) (*Webhook, error) {
	body := map[string]any{"url": url, "is_active": true}
	for _, e := range eventTypes {
		switch e {
		case "issue":
			body["issue"] = true
		case "issue_comment":
			body["issue_comment"] = true
		case "project":
			body["project"] = true
		case "cycle":
			body["cycle"] = true
		case "module":
			body["module"] = true
		}
	}
	var w Webhook
	if err := c.doSession(ctx, "POST", c.sessionPath("/workspaces/%s/webhooks/", c.workspace), body, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// RegenerateWebhookSecret rotates a webhook's HMAC secret and returns the
// new one — the other moment the API reveals it.
func (c *Client) RegenerateWebhookSecret(ctx context.Context, webhookID string) (*Webhook, error) {
	var w Webhook
	if err := c.doSession(ctx, "POST", c.sessionPath("/workspaces/%s/webhooks/%s/regenerate/", c.workspace, webhookID), nil, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// DeleteWebhook removes a workspace webhook.
func (c *Client) DeleteWebhook(ctx context.Context, webhookID string) error {
	return c.doSession(ctx, "DELETE", c.sessionPath("/workspaces/%s/webhooks/%s/", c.workspace, webhookID), nil, nil)
}

// --- HTTP plumbing ------------------------------------------------------------------

// wsPath builds a v1 path scoped to the workspace.
func (c *Client) wsPath(rest string) string {
	return "/api/v1/workspaces/" + c.workspace + rest
}

// projectPath builds a v1 path scoped to the workspace and project.
func (c *Client) projectPath(projectID, rest string) string {
	return c.wsPath("/projects/" + projectID + rest)
}

// itemPath builds a v1 work-item path.
func (c *Client) itemPath(projectID, itemID string) string {
	return c.projectPath(projectID, "/work-items/"+itemID+"/")
}

// sessionPath formats a session-API path (no /v1) with the given args.
func (c *Client) sessionPath(format string, args ...any) string {
	return "/api/" + strings.TrimPrefix(fmt.Sprintf(format, args...), "/")
}

// do performs a v1-API request with the API key and decodes the reply
// into out (if non-nil). 429s (rate limit) are retried with a pause, so
// a single call rides out a throttle window.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if c.apiKey == "" {
		return errors.New("plane: APIKey is required")
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := c.newRequest(ctx, method, path, body, c.apiKey)
		if err != nil {
			return err
		}
		lastErr = c.execute(req, out)
		if !isRateLimited(lastErr) || attempt >= maxRateAttempts-1 {
			return lastErr
		}
		if err := waitRateLimit(ctx); err != nil {
			return err
		}
	}
}

// doSession performs a session-API request, signing in on first use and
// retrying once after a 401/403 (expired session or stale CSRF — neither
// means the request executed, so retrying is safe).
func (c *Client) doSession(ctx context.Context, method, path string, body, out any) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.ensureSession(ctx); err != nil {
			return err
		}
		req, err := c.newRequest(ctx, method, path, body, "")
		if err != nil {
			return err
		}
		if method != "GET" {
			c.mu.Lock()
			csrf := c.csrf
			c.mu.Unlock()
			req.Header.Set("X-CSRFToken", csrf)
		}
		lastErr = c.execute(req, out)
		var apiErr *APIError
		if !errors.As(lastErr, &apiErr) || (apiErr.Status != http.StatusUnauthorized && apiErr.Status != http.StatusForbidden) {
			return lastErr
		}
		c.invalidateSession()
	}
	return lastErr
}

// doList / doSessionList decode list endpoints, which come back as a
// cursor-paginated envelope {"results": [...]} on the v1 API and as a bare
// array on some session endpoints.
func (c *Client) doList(ctx context.Context, method, path string, out any) error {
	if c.apiKey == "" {
		return errors.New("plane: APIKey is required")
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := c.newRequest(ctx, method, path, nil, c.apiKey)
		if err != nil {
			return err
		}
		lastErr = c.executeList(req, out)
		if !isRateLimited(lastErr) || attempt >= maxRateAttempts-1 {
			return lastErr
		}
		if err := waitRateLimit(ctx); err != nil {
			return err
		}
	}
}

func (c *Client) doSessionList(ctx context.Context, method, path string, out any) error {
	if err := c.ensureSession(ctx); err != nil {
		return err
	}
	req, err := c.newRequest(ctx, method, path, nil, "")
	if err != nil {
		return err
	}
	return c.executeList(req, out)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body any, apiKey string) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}
	return req, nil
}

// execute sends req, mapping non-2xx to *APIError and decoding the body
// into out when non-nil.
func (c *Client) execute(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("plane: reading %s %s: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Method: req.Method, Path: req.URL.Path, Status: resp.StatusCode, Body: string(b)}
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("plane: decoding %s %s: %w", req.Method, req.URL.Path, err)
		}
	}
	return nil
}

// executeList is execute for list endpoints, accepting both the
// paginated envelope and a bare array.
func (c *Client) executeList(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("plane: reading %s %s: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Method: req.Method, Path: req.URL.Path, Status: resp.StatusCode, Body: string(b)}
	}
	if err := json.Unmarshal(b, out); err != nil {
		// not a bare array — try the paginated envelope
		var env struct {
			Results json.RawMessage `json:"results"`
		}
		if envErr := json.Unmarshal(b, &env); envErr != nil || env.Results == nil {
			return fmt.Errorf("plane: decoding list %s %s: %w", req.Method, req.URL.Path, err)
		}
		if err := json.Unmarshal(env.Results, out); err != nil {
			return fmt.Errorf("plane: decoding list %s %s: %w", req.Method, req.URL.Path, err)
		}
	}
	return nil
}

// --- session management ---------------------------------------------------------------

// ensureSession signs in (once) so session-API calls are authenticated.
// The flow mirrors scripts/plane-setup.sh: fetch an anonymous CSRF token,
// POST /auth/sign-in/ form-encoded, then re-fetch the CSRF token, which
// after login moves into the session.
func (c *Client) ensureSession(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loggedIn {
		return nil
	}
	csrf, err := c.fetchCSRF(ctx)
	if err != nil {
		return err
	}
	form := url.Values{"email": {c.email}, "password": {c.password}}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/auth/sign-in/", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRFToken", csrf)
	req.Header.Set("Accept", "application/json")
	resp, err := c.login.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	// 2xx is a plain success; 3xx is the post-login redirect, which the
	// login client deliberately does not follow (see New).
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return &APIError{Method: "POST", Path: "/auth/sign-in/", Status: resp.StatusCode, Body: "sign-in failed (check Email/Password)"}
	}
	if !sessionCookieIn(resp) && !c.hasSessionCookie() {
		return errors.New("plane: sign-in accepted but no session cookie was set")
	}
	// The CSRF token moves into the session after login — re-fetch it.
	if csrf, err = c.fetchCSRF(ctx); err != nil {
		return err
	}
	c.csrf = csrf
	c.loggedIn = true
	return nil
}

func (c *Client) invalidateSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loggedIn = false
	c.csrf = ""
}

// fetchCSRF GETs the public CSRF endpoint, which returns
// {"csrf_token": "..."} (and seeds the csrftoken cookie the jar keeps).
func (c *Client) fetchCSRF(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/auth/get-csrf-token/", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", &APIError{Method: "GET", Path: "/auth/get-csrf-token/", Status: resp.StatusCode, Body: string(b)}
	}
	var out struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.CSRFToken == "" {
		return "", errors.New("plane: no csrf_token in /auth/get-csrf-token/ response")
	}
	return out.CSRFToken, nil
}

// sessionCookieIn reports whether the response itself sets a Plane
// session cookie. This is checked in addition to the jar, because a
// Set-Cookie with no Path attribute is stored scoped to the request
// path, where Jar.Cookies on the base URL would not find it.
func sessionCookieIn(resp *http.Response) bool {
	for _, h := range resp.Header["Set-Cookie"] {
		cks, err := http.ParseCookie(h)
		if err != nil {
			continue
		}
		for _, ck := range cks {
			if ck.Name == "session-id" || ck.Name == "sessionid" {
				return true
			}
		}
	}
	return false
}

// hasSessionCookie reports whether the jar holds a session cookie for this
// instance. Recent Plane builds name it "session-id", older ones
// "sessionid".
func (c *Client) hasSessionCookie() bool {
	u, err := url.Parse(c.baseURL)
	if err != nil || c.http.Jar == nil {
		return false
	}
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == "session-id" || ck.Name == "sessionid" {
			return true
		}
	}
	return false
}

// --- small set helpers ----------------------------------------------------------------

// unionStrings returns a in order, followed by any of add not already in
// a (case-sensitive, IDs are exact).
func unionStrings(a, add []string) []string {
	seen := make(map[string]bool, len(a)+len(add))
	out := make([]string, 0, len(a)+len(add))
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range add {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// minusStrings returns a without any of drop.
func minusStrings(a, drop []string) []string {
	d := make(map[string]bool, len(drop))
	for _, s := range drop {
		d[s] = true
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if !d[s] {
			out = append(out, s)
		}
	}
	return out
}
