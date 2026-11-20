// The owner's control surface for the PR review queues (Job 5 of
// docs/pr-review-build-plan.md): !review <pr> <reviewer> requests a
// review of an open PR — the record goes into reviewstate and one job
// lands on the reviewer's queue (internal/reviewroute); a reviewer whose
// config entry has needs_release (Fable) is HELD instead, and
// !review-release <pr> publishes the held review.
//
// It is owner-only by construction: the chat layer only fires the
// OnMessage hook for the owner DM (internal/chat/discord.go), so no
// non-owner message can ever reach handleReview — the same guarantee
// that protects !pause, !restart and !fullauto.
package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewcfg"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewrequest"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewroute"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

// cutReview splits a !review command out of a chat message.
//
//	isCommand is true when the message starts with "!review" followed by a
//	word boundary — "!reviewx" is not a command and falls through to the
//	brain, and "!review-release" never matches "!review" (the character
//	after the word is not a boundary).
//
//	valid is true only when the argument is exactly two tokens: a positive
//	integer PR number and a reviewer id (lowercased, case-insensitive).
//	Anything else is still isCommand (the owner clearly meant the command)
//	but valid is false, so the handler answers with usage instead of
//	sending it to the brain. pr and reviewer are only meaningful when
//	valid is true.
func cutReview(content string) (pr int, reviewer string, valid, isCommand bool) {
	rest, ok := cutCmdPrefix(content, "!review")
	if !ok {
		return 0, "", false, false
	}
	fields := strings.Fields(rest)
	if len(fields) != 2 {
		return 0, "", false, true // the command, but the argument is missing or wrong
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n <= 0 {
		return 0, "", false, true
	}
	return n, strings.ToLower(fields[1]), true, true
}

// cutReviewRelease splits a !review-release command out of a chat message.
// It accepts only "!review-release <number>". The same boundary rule
// applies: "!review-releasex" is not a command, and "!review" never
// claims a release.
func cutReviewRelease(content string) (pr int, valid, isCommand bool) {
	rest, ok := cutCmdPrefix(content, "!review-release")
	if !ok {
		return 0, false, false
	}
	fields := strings.Fields(rest)
	if len(fields) != 1 {
		return 0, false, true // the command, but the argument is missing or wrong
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n <= 0 {
		return 0, false, true
	}
	return n, true, true
}

// handleReview handles !review and !review-release. It reports whether
// the message was one of the two commands, so the caller does not also
// send it to the brain.
func handleReview(d sender, content string) bool {
	if pr, reviewer, valid, isCommand := cutReview(content); isCommand {
		handleReviewCommand(d, pr, reviewer, valid)
		return true
	}
	if pr, valid, isCommand := cutReviewRelease(content); isCommand {
		handleReviewRelease(d, pr, valid)
		return true
	}
	return false
}

// handleReviewCommand executes !review <pr> <reviewer>. The reviewer is
// checked against the review config first (an unknown name is replied
// with the reviewroute error and nothing is published), then the PR's
// head branch and SHA come from GitHub, then the reviewrequest service
// records and publishes — or holds, for a needs_release reviewer. The
// held reply is the exact one from the plan; a success reply names the
// queue; on error the reply is the error text and nothing is published.
func handleReviewCommand(d sender, pr int, reviewer string, valid bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if !valid {
		_ = d.Notify(ctx, "Usage: !review <pr> <reviewer>   (e.g. !review 268 sonnet)   —   !review-release <pr> releases a held Fable review")
		return
	}
	cfg, err := loadReviewConfig()
	if err != nil {
		_ = d.Notify(ctx, "I can't read the review config, so I can't route that review.")
		return
	}
	q, err := reviewroute.Queue(reviewer, cfg)
	if err != nil {
		_ = d.Notify(ctx, err.Error())
		return
	}
	branch, head, err := githubPRInfo(ctx, pr)
	if err != nil {
		_ = d.Notify(ctx, err.Error())
		return
	}
	svc := newReviewService(cfg)
	_, held, err := svc.RequestReview(ctx, pr, branch, reviewer, "", head)
	if err != nil {
		_ = d.Notify(ctx, err.Error())
		return
	}
	if held {
		_ = d.Notify(ctx, fmt.Sprintf("Fable review for PR %d is held. Release it with !review-release %d", pr, pr))
		return
	}
	_ = d.Notify(ctx, fmt.Sprintf("Review requested: %s will review PR %d (%s) on queue %s.", reviewer, pr, branch, q))
}

// handleReviewRelease executes !review-release <pr>: it publishes the
// one message for a held request and clears Held. Releasing a record
// that is not held (or missing) replies with the error text.
func handleReviewRelease(d sender, pr int, valid bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if !valid {
		_ = d.Notify(ctx, "Usage: !review-release <pr>   (releases a held Fable review)")
		return
	}
	cfg, err := loadReviewConfig()
	if err != nil {
		_ = d.Notify(ctx, "I can't read the review config, so I can't release that review.")
		return
	}
	if err := newReviewService(cfg).Release(ctx, pr); err != nil {
		_ = d.Notify(ctx, err.Error())
		return
	}
	_ = d.Notify(ctx, fmt.Sprintf("Released: the held review for PR %d is on its way.", pr))
}

// newReviewService wires the sender side of a review request with the
// real queue publish (internal/queue via RABBITMQ_URL). Tests override
// publishReview — the same overridable-var idiom as nobrakes.UnitActive.
func newReviewService(cfg reviewcfg.Config) *reviewrequest.Service {
	return &reviewrequest.Service{
		Dir:     reviewstate.DefaultDir(),
		Cfg:     cfg,
		Publish: publishReview,
	}
}

// publishReview delivers one review message to a queue by name.
var publishReview func(ctx context.Context, m queue.Message, q string) error = func(ctx context.Context, m queue.Message, q string) error {
	conn, err := queue.NewConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Publish(m, q)
}

// githubPRInfo fetches the head branch and head SHA of PR pr in the
// build repo. The chat container has no gh CLI, no ghapp-token helper
// and no token file, so this mints a GitHub App installation token
// itself (pure Go, the same dance as the ghapp-token shell helper): an
// RS256 JWT signed with the app key from the mounted ~/.config/ghapp,
// exchanged for an installation token. $GITHUB_TOKEN, when set, is used
// instead of minting; $GITHUB_API_URL overrides the API base (tests).
func githubPRInfo(ctx context.Context, pr int) (branch, head string, err error) {
	tok, err := ghToken(ctx)
	if err != nil {
		return "", "", err
	}
	var pull struct {
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := ghAPI(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", githubRepo, pr), "Bearer "+tok, &pull); err != nil {
		return "", "", err
	}
	if pull.Head.Ref == "" || pull.Head.SHA == "" {
		return "", "", fmt.Errorf("review: GitHub did not return a head for PR %d", pr)
	}
	return pull.Head.Ref, pull.Head.SHA, nil
}

// githubRepo is the build repo the PRs live in (the same repo
// scripts/jarvis-review.sh passes to gh).
const githubRepo = "ImMakingAJarvis/ai-agent-build"

// ghHTTPClient bounds every GitHub call: a hung API must not hold the
// owner's reply hostage past the command's own timeout.
var ghHTTPClient = &http.Client{Timeout: 20 * time.Second}

// ghAPI runs one request against the (possibly overridden) GitHub API
// base and decodes the JSON body into out.
func ghAPI(ctx context.Context, method, path, auth string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, ghBaseURL()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := ghHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func ghBaseURL() string {
	if u := os.Getenv("GITHUB_API_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://api.github.com"
}

// ghToken returns the token for GitHub calls: $GITHUB_TOKEN when set,
// otherwise a minted installation token.
func ghToken(ctx context.Context) (string, error) {
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		return tok, nil
	}
	return ghInstallationToken(ctx)
}

// ghTokenCache holds the minted installation token until it is close to
// expiry, so repeated commands do not re-mint (the ghapp-token helper
// does the same with its token.cache file).
var ghTokenCache struct {
	sync.Mutex
	token      string
	validUntil time.Time
}

// ghInstallationToken mints a GitHub App installation token: read the
// app credentials (GHAPP_ID / GHAPP_ORG from the mounted env file, the
// RSA key from key.pem), sign a short-lived JWT (RS256), look up the
// installation for the org, and exchange it for an installation token
// (valid ~9 minutes; refreshed here at 5).
func ghInstallationToken(ctx context.Context) (string, error) {
	ghTokenCache.Lock()
	defer ghTokenCache.Unlock()
	if ghTokenCache.token != "" && time.Now().Before(ghTokenCache.validUntil) {
		return ghTokenCache.token, nil
	}
	appID, org, err := ghAppEnv()
	if err != nil {
		return "", err
	}
	jwt, err := ghAppJWT(appID)
	if err != nil {
		return "", err
	}
	var inst struct {
		ID int `json:"id"`
	}
	if err := ghAPI(ctx, http.MethodGet, "/orgs/"+org+"/installation", "Bearer "+jwt, &inst); err != nil {
		return "", err
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := ghAPI(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), "Bearer "+jwt, &tok); err != nil {
		return "", err
	}
	if tok.Token == "" {
		return "", fmt.Errorf("github installation token: empty token in response")
	}
	ghTokenCache.token = tok.Token
	ghTokenCache.validUntil = time.Now().Add(5 * time.Minute)
	return ghTokenCache.token, nil
}

// ghAppConfDir is where the GitHub App credentials live: the compose
// mount of ~/.config/ghapp (key.pem, env with GHAPP_ID / GHAPP_ORG),
// overridable with $GHAPP_CONF (tests).
func ghAppConfDir() string {
	if dir := os.Getenv("GHAPP_CONF"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/home/agent"
	}
	return filepath.Join(home, ".config", "ghapp")
}

// ghAppEnv reads GHAPP_ID and GHAPP_ORG from the app env file (shell
// KEY=VALUE lines; the same file the ghapp-token helper sources).
func ghAppEnv() (appID, org string, err error) {
	path := filepath.Join(ghAppConfDir(), "env")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("github app env: %w", err)
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vals[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	if vals["GHAPP_ID"] == "" || vals["GHAPP_ORG"] == "" {
		return "", "", fmt.Errorf("github app env: GHAPP_ID and GHAPP_ORG are required in %s", path)
	}
	return vals["GHAPP_ID"], vals["GHAPP_ORG"], nil
}

// ghAppJWT builds the app JWT for the installation-token exchange:
// header {"alg":"RS256","typ":"JWT"}, payload with iat 60 s in the past,
// exp 9 minutes out and iss the app id, signed with the app key.
func ghAppJWT(appID string) (string, error) {
	keyPEM, err := os.ReadFile(filepath.Join(ghAppConfDir(), "key.pem"))
	if err != nil {
		return "", fmt.Errorf("github app key: %w", err)
	}
	key, err := parseRSAKey(keyPEM)
	if err != nil {
		return "", err
	}
	now := time.Now()
	payload, err := json.Marshal(struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}{
		Iat: now.Add(-60 * time.Second).Unix(),
		Exp: now.Add(9 * time.Minute).Unix(),
		Iss: appID,
	})
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding
	signing := b64.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + b64.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64.EncodeToString(sig), nil
}

// parseRSAKey accepts the app key in PKCS#8 or PKCS#1 form (both are
// produced by `openssl genpkey` / `openssl genrsa`).
func parseRSAKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("github app key: no PEM block in key.pem")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("github app key: not an RSA key")
		}
		return rsaKey, nil
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	return key, nil
}
