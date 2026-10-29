package photomind

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/art"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// fakeBrain is a scripted thinking mind.
type fakeBrain struct {
	replies []string // one reply per Chat call, in order
	err     error
	healthy bool
	calls   [][]models.ChatMessage
}

func (f *fakeBrain) Chat(model string, msgs []models.ChatMessage) (string, error) {
	f.calls = append(f.calls, msgs)
	if f.err != nil {
		return "", f.err
	}
	i := len(f.calls) - 1
	if i >= len(f.replies) {
		return "", errors.New("fakeBrain: no scripted reply left")
	}
	return f.replies[i], nil
}

func (f *fakeBrain) Healthcheck() bool { return f.healthy }

// fakeGen is a scripted generator that records the prompts it receives.
type fakeGen struct {
	prompts []string
	err     error
}

func (f *fakeGen) Generate(ctx context.Context, prompt string) (*art.Image, error) {
	f.prompts = append(f.prompts, prompt)
	if f.err != nil {
		return nil, f.err
	}
	return &art.Image{Path: "img-" + strings.Join(f.prompts, "+"), Seed: int64(len(f.prompts))}, nil
}

func TestCleanPrompt(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"A photo of a red panda in fresh snow.", "A photo of a red panda in fresh snow."},
		{"  A photo of a red panda.  ", "A photo of a red panda."},
		{`"A photo of a red panda in fresh snow, soft light."`, "A photo of a red panda in fresh snow, soft light."},
		{"Prompt: A photo of a red panda.", "A photo of a red panda."},
		{"Improved prompt: A photo of a red panda.", "A photo of a red panda."},
		{"Prompt:\nA photo of a red panda.", "A photo of a red panda."},
		{"```prompt\nA photo of a red panda.\n```", "A photo of a red panda."},
		{"```\nA photo of a red panda.\n```", "A photo of a red panda."},
		{"A photo of a red panda.\n\nHere is why I chose that: ...", "A photo of a red panda."},
		{"A photo of Prompt City at night.", "A photo of Prompt City at night."}, // no label without a colon
	}
	for _, c := range cases {
		got, err := cleanPrompt(c.in)
		if err != nil {
			t.Errorf("cleanPrompt(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("cleanPrompt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := cleanPrompt("   "); err == nil {
		t.Error("cleanPrompt(empty) should fail")
	}
	long := strings.Repeat("a very long prompt line ", 100)
	if got, _ := cleanPrompt(long); len([]rune(got)) > 1500 {
		t.Errorf("cleanPrompt did not cap length: %d runes", len([]rune(got)))
	}
}

func TestAssistSendsRequestToBrain(t *testing.T) {
	brain := &fakeBrain{replies: []string{"A photo of a red panda in fresh snow, dawn light."}}
	svc := New(brain, "jarvis-brain", &fakeGen{})

	got, err := svc.Assist(context.Background(), "red panda in snow")
	if err != nil {
		t.Fatal(err)
	}
	if got != "A photo of a red panda in fresh snow, dawn light." {
		t.Errorf("Assist = %q", got)
	}
	if len(brain.calls) != 1 {
		t.Fatalf("expected 1 brain call, got %d", len(brain.calls))
	}
	msgs := brain.calls[0]
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Errorf("unexpected messages: %+v", msgs)
	}
	if msgs[1].Content != "red panda in snow" {
		t.Errorf("user message = %q", msgs[1].Content)
	}
}

func TestGenerateAssistedUsesExpandedPrompt(t *testing.T) {
	expanded := "A photo of a red panda in fresh snow, dawn light, 85mm."
	brain := &fakeBrain{replies: []string{expanded}}
	gen := &fakeGen{}
	svc := New(brain, "jarvis-brain", gen)

	img, used, err := svc.GenerateAssisted(context.Background(), "red panda in snow")
	if err != nil {
		t.Fatal(err)
	}
	if used != expanded {
		t.Errorf("used prompt = %q", used)
	}
	if len(gen.prompts) != 1 || gen.prompts[0] != expanded {
		t.Errorf("generator got %v, want the expanded prompt", gen.prompts)
	}
	if img.Seed != 1 {
		t.Errorf("image seed = %d", img.Seed)
	}
}

func TestCheckIteratesTwice(t *testing.T) {
	p1 := "A photo of a red panda."
	p2 := "A photo of a red panda in fresh snow, dawn light, close-up."
	brain := &fakeBrain{replies: []string{p1, p2}}
	gen := &fakeGen{}
	svc := New(brain, "jarvis-brain", gen)

	res, err := svc.Check(context.Background(), "red panda in snow")
	if err != nil {
		t.Fatal(err)
	}
	if len(gen.prompts) != 2 || gen.prompts[0] != p1 || gen.prompts[1] != p2 {
		t.Errorf("generator got %v", gen.prompts)
	}
	if res.Prompt != p2 || res.Image.Seed != 2 {
		t.Errorf("result = %+v", res)
	}
	if len(res.Drafts) != 2 {
		t.Errorf("drafts = %v", res.Drafts)
	}
	// The critique call must carry both the request and the first prompt.
	critique := brain.calls[1][1].Content
	if !strings.Contains(critique, "red panda in snow") || !strings.Contains(critique, p1) {
		t.Errorf("critique message = %q", critique)
	}
}

func TestCheckSkipsSecondPassWhenUnchanged(t *testing.T) {
	p := "A photo of a red panda in fresh snow, dawn light."
	brain := &fakeBrain{replies: []string{p, p}} // critique sees nothing to fix
	gen := &fakeGen{}
	svc := New(brain, "jarvis-brain", gen)

	res, err := svc.Check(context.Background(), "red panda in snow")
	if err != nil {
		t.Fatal(err)
	}
	if len(gen.prompts) != 1 {
		t.Errorf("expected 1 generation, got %d", len(gen.prompts))
	}
	if res.Prompt != p || len(res.Drafts) != 1 {
		t.Errorf("result = %+v", res)
	}
}

func TestCheckSecondGenerationFailsReturnsFirstImage(t *testing.T) {
	p1 := "A photo of a red panda."
	p2 := "A photo of a red panda, improved."
	brain := &fakeBrain{replies: []string{p1, p2}}
	gen := &fakeGen{}
	svc := New(brain, "jarvis-brain", gen)

	// Let the first generation succeed, fail the second.
	svc.Art = &secondGenFail{inner: gen, failAfter: 1, err: errors.New("art busy")}

	res, err := svc.Check(context.Background(), "red panda in snow")
	if err == nil {
		t.Fatal("expected an error from the second pass")
	}
	if res == nil || res.Image.Seed != 1 {
		t.Fatalf("expected the first image back, got %+v", res)
	}
}

// secondGenFail passes calls through to inner but fails after n of them.
type secondGenFail struct {
	inner     Gen
	failAfter int
	err       error
	n         int
}

func (s *secondGenFail) Generate(ctx context.Context, prompt string) (*art.Image, error) {
	s.n++
	if s.n > s.failAfter {
		return nil, s.err
	}
	return s.inner.Generate(ctx, prompt)
}

func TestCheckFirstGenerationFails(t *testing.T) {
	brain := &fakeBrain{replies: []string{"A photo of a red panda."}}
	svc := New(brain, "jarvis-brain", &fakeGen{err: errors.New("art down")})

	res, err := svc.Check(context.Background(), "red panda")
	if err == nil {
		t.Fatal("expected an error")
	}
	if res != nil {
		t.Errorf("expected no result, got %+v", res)
	}
}

func TestHealthyReflectsBrainHealthcheck(t *testing.T) {
	if svc := New(&fakeBrain{healthy: true}, "m", &fakeGen{}); !svc.Healthy() {
		t.Error("expected healthy")
	}
	if svc := New(&fakeBrain{healthy: false}, "m", &fakeGen{}); svc.Healthy() {
		t.Error("expected unhealthy")
	}
	// A brain without a Healthcheck method is assumed up.
	if svc := New(staticBrain{}, "m", &fakeGen{}); !svc.Healthy() {
		t.Error("expected assumed-up brain to be healthy")
	}
}

type staticBrain struct{}

func (staticBrain) Chat(model string, msgs []models.ChatMessage) (string, error) {
	return "A photo of something.", nil
}
