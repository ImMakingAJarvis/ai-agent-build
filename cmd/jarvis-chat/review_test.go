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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
)

func TestReviewCmd(t *testing.T) {
	cases := []struct {
		in        string
		pr        int
		reviewer  string
		valid     bool
		isCommand bool
	}{
		{"!review 268 sonnet", 268, "sonnet", true, true},
		{"!review 268 SONNET", 268, "sonnet", true, true}, // case-insensitive, lowercased
		{"!review", 0, "", false, true},
		{"!review 268", 0, "", false, true},
		{"!review abc sonnet", 0, "", false, true},
		{"hello !review 268 sonnet", 0, "", false, false}, // must start the message
		{"!reviewx 268 sonnet", 0, "", false, false},      // word boundary required
	}
	for _, c := range cases {
		pr, reviewer, valid, isCommand := cutReview(c.in)
		if pr != c.pr || reviewer != c.reviewer || valid != c.valid || isCommand != c.isCommand {
			t.Errorf("cutReview(%q) = pr=%d reviewer=%q valid=%v isCommand=%v; want pr=%d reviewer=%q valid=%v isCommand=%v",
				c.in, pr, reviewer, valid, isCommand, c.pr, c.reviewer, c.valid, c.isCommand)
		}
		// the release is a different command: !review must not claim it.
		if _, _, _, is := cutReview("!review-release 268"); is {
			t.Error("cutReview claimed !review-release")
		}
	}
	// and the reverse, plus the NoBrakes parsers: none claims !review.
	for _, in := range []string{"!review 268 sonnet", "!review 268", "!review"} {
		if _, _, is := cutReviewRelease(in); is {
			t.Errorf("cutReviewRelease(%q) claimed !review", in)
		}
		if _, _, _, is := cutFullAuto(in); is {
			t.Errorf("cutFullAuto(%q) claimed !review", in)
		}
	}
}

func TestCutReviewRelease(t *testing.T) {
	cases := []struct {
		in        string
		pr        int
		valid     bool
		isCommand bool
	}{
		{"!review-release 268", 268, true, true},
		{"!review-release 268 ", 268, true, true},
		{"!review-release", 0, false, true},
		{"!review-release abc", 0, false, true},
		{"!review-release -3", 0, false, true},
		{"!review-release 268 sonnet", 0, false, true}, // only <number>
		{"hello !review-release 268", 0, false, false},
		{"!review-releasex 268", 0, false, false}, // word boundary required
	}
	for _, c := range cases {
		pr, valid, isCommand := cutReviewRelease(c.in)
		if pr != c.pr || valid != c.valid || isCommand != c.isCommand {
			t.Errorf("cutReviewRelease(%q) = pr=%d valid=%v isCommand=%v; want pr=%d valid=%v isCommand=%v",
				c.in, pr, valid, isCommand, c.pr, c.valid, c.isCommand)
		}
	}
}

// reviewConfigSonnetFable is the two-reviewer block the handler tests
// run against: sonnet publishes, fable holds (needs_release).
const reviewConfigSonnetFable = "  sonnet:\n    queue: review.sonnet\n" +
	"  fable:\n    queue: review.fable\n    needs_release: true\n"

// fakeGitHubPR points $GITHUB_API_URL at a stub that answers the PR
// lookup, and sets $GITHUB_TOKEN so no app token is minted.
func fakeGitHubPR(t *testing.T, ref, sha string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"head":{"ref":%q,"sha":%q}}`, ref, sha)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GITHUB_TOKEN", "test-token")
}

type publishCall struct {
	m queue.Message
	q string
}

// fakePublish records publish calls instead of dialing RabbitMQ.
func fakePublish(t *testing.T) *[]publishCall {
	t.Helper()
	calls := []publishCall{}
	old := publishReview
	t.Cleanup(func() { publishReview = old })
	publishReview = func(ctx context.Context, m queue.Message, q string) error {
		calls = append(calls, publishCall{m: m, q: q})
		return nil
	}
	return &calls
}

func TestHandleReviewHeldFable(t *testing.T) {
	writeReviewConfig(t, reviewConfigSonnetFable)
	dataDir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dataDir)
	fakeGitHubPR(t, "phase-x", "abc1234")
	calls := fakePublish(t)
	d := &testDiscord{}
	handleReview(d, "!review 268 fable")

	want := "Fable review for PR 268 is held. Release it with !review-release 268"
	if msg := d.lastNotify(); msg != want {
		t.Errorf("reply %q; want exactly %q", msg, want)
	}
	if len(*calls) != 0 {
		t.Errorf("held review published %d message(s); want none", len(*calls))
	}
	rec, err := reviewstate.Load(filepath.Join(dataDir, "review"), 268)
	if err != nil {
		t.Fatalf("record not saved: %v", err)
	}
	if !rec.Held {
		t.Error("record Held = false; want true")
	}
	if rec.Branch != "phase-x" || rec.Head != "abc1234" {
		t.Errorf("record = %s/%s; want phase-x/abc1234", rec.Branch, rec.Head)
	}
}

func TestHandleReviewSuccess(t *testing.T) {
	writeReviewConfig(t, reviewConfigSonnetFable)
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	fakeGitHubPR(t, "phase-x", "abc1234")
	calls := fakePublish(t)
	d := &testDiscord{}
	handleReview(d, "!review 268 sonnet")

	if len(*calls) != 1 {
		t.Fatalf("published %d message(s); want 1", len(*calls))
	}
	call := (*calls)[0]
	if call.q != "review.sonnet" {
		t.Errorf("queue %q; want review.sonnet", call.q)
	}
	if call.m.Payload["pr"] != "268" || call.m.Payload["branch"] != "phase-x" || call.m.Payload["reviewer"] != "sonnet" {
		t.Errorf("payload = %v; want pr=268 branch=phase-x reviewer=sonnet", call.m.Payload)
	}
	if msg := d.lastNotify(); !strings.Contains(msg, "review.sonnet") {
		t.Errorf("reply %q should name the queue", msg)
	}
}

func TestHandleReviewUnknownReviewer(t *testing.T) {
	writeReviewConfig(t, sonnetReviewerYAML) // fable is NOT in this config
	calls := fakePublish(t)
	d := &testDiscord{}
	handleReview(d, "!review 268 fable")

	if msg := d.lastNotify(); !strings.Contains(msg, "unknown reviewer") {
		t.Errorf("reply %q should say the reviewer is unknown", msg)
	}
	if len(*calls) != 0 {
		t.Errorf("published %d message(s); want none", len(*calls))
	}
}

func TestHandleReviewUsage(t *testing.T) {
	for _, in := range []string{"!review", "!review 268", "!review 0 sonnet", "!review abc sonnet", "!review 268 sonnet extra", "!review-release", "!review-release 268 x"} {
		d := &testDiscord{}
		if !handleReview(d, in) {
			t.Errorf("%q: handleReview = false; want true", in)
			continue
		}
		if msg := d.lastNotify(); !strings.Contains(msg, "Usage") {
			t.Errorf("%q: reply %q should be usage", in, msg)
		}
	}
}

func TestHandleReviewGitHubError(t *testing.T) {
	writeReviewConfig(t, reviewConfigSonnetFable)
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GITHUB_TOKEN", "test-token")
	calls := fakePublish(t)
	d := &testDiscord{}
	handleReview(d, "!review 999999 sonnet")

	if msg := d.lastNotify(); !strings.Contains(msg, "404") {
		t.Errorf("reply %q should carry the error text", msg)
	}
	if len(*calls) != 0 {
		t.Errorf("published %d message(s) after a GitHub error; want none", len(*calls))
	}
}

func TestHandleReviewNotACommand(t *testing.T) {
	d := &testDiscord{}
	if handleReview(d, "hello !review 268 sonnet") {
		t.Error("a message with !review inside is not a command")
	}
	if n := d.notifyCount(); n != 0 {
		t.Errorf("non-command notified %d time(s); want 0", n)
	}
}

func TestHandleReviewRelease(t *testing.T) {
	writeReviewConfig(t, reviewConfigSonnetFable)
	dataDir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dataDir)
	dir := filepath.Join(dataDir, "review")
	// A held Fable record for PR 268.
	if err := reviewstate.Save(dir, reviewstate.Record{
		PR: 268, Branch: "phase-x", Head: "abc1234",
		ReviewerModel: "fable", Token: "fable", Held: true,
	}); err != nil {
		t.Fatal(err)
	}
	calls := fakePublish(t)
	d := &testDiscord{}
	handleReview(d, "!review-release 268")

	if len(*calls) != 1 {
		t.Fatalf("published %d message(s); want 1", len(*calls))
	}
	if call := (*calls)[0]; call.q != "review.fable" || call.m.Payload["pr"] != "268" {
		t.Errorf("release published %s %v; want review.fable pr=268", call.q, call.m.Payload)
	}
	rec, err := reviewstate.Load(dir, 268)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Held {
		t.Error("record still Held after release")
	}
	if msg := d.lastNotify(); !strings.Contains(msg, "Released") {
		t.Errorf("reply %q should say released", msg)
	}
}

func TestHandleReviewReleaseNotHeld(t *testing.T) {
	writeReviewConfig(t, reviewConfigSonnetFable)
	dataDir := t.TempDir()
	t.Setenv("JARVIS_DATA_DIR", dataDir)
	dir := filepath.Join(dataDir, "review")
	// A record that is NOT held.
	if err := reviewstate.Save(dir, reviewstate.Record{
		PR: 268, Branch: "phase-x", Head: "abc1234",
		ReviewerModel: "sonnet", Token: "sonnet",
	}); err != nil {
		t.Fatal(err)
	}
	calls := fakePublish(t)
	d := &testDiscord{}
	handleReview(d, "!review-release 268")

	if len(*calls) != 0 {
		t.Errorf("published %d message(s) for a non-held record; want none", len(*calls))
	}
	if msg := d.lastNotify(); !strings.Contains(msg, "held") {
		t.Errorf("reply %q should say the record is not held", msg)
	}
}

func TestHandleReviewReleaseMissing(t *testing.T) {
	writeReviewConfig(t, reviewConfigSonnetFable)
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	calls := fakePublish(t)
	d := &testDiscord{}
	handleReview(d, "!review-release 268")

	if len(*calls) != 0 {
		t.Errorf("published %d message(s) for a missing record; want none", len(*calls))
	}
	if msg := d.lastNotify(); !strings.Contains(msg, "no review record") {
		t.Errorf("reply %q should say there is no record", msg)
	}
}

// TestGitHubAppTokenMint exercises the mint path end to end against a
// stub: app env + a fresh RSA key (GHAPP_CONF), the installation lookup
// and the token exchange, and the RS256 signature of the app JWT
// verified with the key's public half.
func TestGitHubAppTokenMint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GHAPP_CONF", dir)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte("GHAPP_ID=12345\nGHAPP_ORG=ImMakingAJarvis\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var jwtAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer " {
			t.Error("github stub: empty bearer token")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/orgs/ImMakingAJarvis/installation":
			jwtAuth = r.Header.Get("Authorization")
			fmt.Fprint(w, `{"id": 777}`)
		case "/app/installations/777/access_tokens":
			if r.Header.Get("Authorization") != jwtAuth {
				t.Error("token exchange used a different JWT than the installation lookup")
			}
			fmt.Fprint(w, `{"token": "ghs_test"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_API_URL", srv.URL)
	t.Setenv("GITHUB_TOKEN", "") // force the mint path

	// Clear the in-memory cache so this test really mints.
	ghTokenCache.Lock()
	ghTokenCache.token = ""
	ghTokenCache.validUntil = time.Time{}
	ghTokenCache.Unlock()
	t.Cleanup(func() {
		ghTokenCache.Lock()
		ghTokenCache.token = ""
		ghTokenCache.validUntil = time.Time{}
		ghTokenCache.Unlock()
	})

	tok, err := ghToken(context.Background())
	if err != nil {
		t.Fatalf("ghToken: %v", err)
	}
	if tok != "ghs_test" {
		t.Fatalf("token %q; want ghs_test", tok)
	}

	// Verify the app JWT: shape, iss, and the RS256 signature.
	parts := strings.Split(strings.TrimPrefix(jwtAuth, "Bearer "), ".")
	if len(parts) != 3 {
		t.Fatalf("JWT %q: want three dot-separated parts", jwtAuth)
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("JWT payload: %v", err)
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "12345" {
		t.Errorf("JWT iss %q; want 12345", claims.Iss)
	}
	if claims.Exp <= claims.Iat {
		t.Errorf("JWT exp %d <= iat %d", claims.Exp, claims.Iat)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("JWT signature: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Errorf("JWT signature does not verify: %v", err)
	}
}
