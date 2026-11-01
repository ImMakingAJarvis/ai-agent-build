// Package photomind sends photo requests through the thinking mind
// (the local LLM brain) before they reach the art service, so the image
// generator gets a rich, detailed prompt instead of the owner's casual
// words. It also runs the photocheck loop: draft a prompt, generate,
// critique the prompt against the original request, improve it, and
// generate again.
package photomind

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/art"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/models"
)

// Chat is the thinking mind. *models.Client satisfies it.
type Chat interface {
	Chat(model string, messages []models.ChatMessage) (string, error)
}

// Gen is the image generator. *art.Client satisfies it.
type Gen interface {
	Generate(ctx context.Context, prompt string) (*art.Image, error)
}

// Service wires a brain and a generator together.
type Service struct {
	Brain Chat
	Model string
	Art   Gen
}

// New builds a service. model is the brain model name (e.g.
// "jarvis-brain"); it is not needed by the art service.
func New(brain Chat, model string, gen Gen) *Service {
	return &Service{Brain: brain, Model: model, Art: gen}
}

// Healthy reports whether the brain can be reached, so the caller can
// fall back to the owner's raw words when it is down. A brain that does
// not expose a Healthcheck method is assumed to be up.
func (s *Service) Healthy() bool {
	if h, ok := s.Brain.(interface{ Healthcheck() bool }); ok {
		return h.Healthcheck()
	}
	return true
}

// assistSystem is the prompt that turns a casual request into an image
// prompt. scripts/jarvis-art (the "enhance" subcommand) keeps a copy of
// this text for the CLI; keep the two in sync.
const assistSystem = `You write prompts for Z-Image-Turbo, a text-to-image model that renders a square 1024x1024 photo.
The user's message is a short, casual photo request. Turn it into one rich, concrete prompt the model can follow:
- keep the user's core subject and intent exactly;
- add specific visual detail: setting, time of day, natural lighting and atmosphere, composition (shot type, angle), camera or lens style, color palette, mood;
- default to photographic realism ("a photo of ...") unless the user asks for another style;
- never put written text, captions, logos or watermarks in the scene (the model renders text badly);
- if the user's request is already a detailed prompt, return it almost unchanged, with only light polish.
Reply with the prompt only: no quotes, no labels, no preamble, one paragraph, under 120 words.`

// checkSystem is the prompt that makes the mind critique its own prompt
// (it cannot see the image, so it judges the prompt against the
// request). Keep in sync with scripts/jarvis-art where useful.
const checkSystem = `You are reviewing a prompt you wrote for a text-to-image model (Z-Image-Turbo, square 1024x1024 photos). The image was generated, but you cannot see it, so you judge the prompt itself.
Given the user's original request and the prompt that was used, ask:
- what of the request is missing, vague, or at risk of being misread by the model?
- which concrete visual detail (lighting, composition, subject detail, mood) could be sharpened?
Keep everything that was right and produce the improved prompt.
Reply with the improved prompt only: no quotes, no labels, no preamble, one paragraph, under 120 words.`

// Assist turns a casual photo request into a detailed image prompt by
// asking the thinking mind to expand it.
func (s *Service) Assist(ctx context.Context, request string) (string, error) {
	out, err := s.Brain.Chat(s.Model, []models.ChatMessage{
		{Role: "system", Content: assistSystem},
		{Role: "user", Content: request},
	})
	if err != nil {
		return "", fmt.Errorf("photomind: thinking mind: %w", err)
	}
	p, err := cleanPrompt(out)
	if err != nil {
		return "", fmt.Errorf("photomind: %w", err)
	}
	return p, nil
}

// GenerateAssisted runs Assist and then generates the photo with the
// resulting prompt. It returns the image and the prompt actually sent
// to the art service.
func (s *Service) GenerateAssisted(ctx context.Context, request string) (*art.Image, string, error) {
	p, err := s.Assist(ctx, request)
	if err != nil {
		return nil, "", err
	}
	img, err := s.Art.Generate(ctx, p)
	if err != nil {
		return nil, p, err
	}
	return img, p, nil
}

// CheckResult is the outcome of a photocheck loop.
type CheckResult struct {
	Image  *art.Image // the image from the last successful pass
	Prompt string     // the prompt that produced Image
	Drafts []string   // every prompt tried, in order
}

// Check runs the iterate loop: draft a prompt, generate, critique the
// prompt against the original request, improve it, generate again. If
// the critique finds nothing to change the first image stands and no
// second generation is spent. If a later pass fails after an image
// already exists, that image is returned together with the error so the
// caller can still show the owner something.
func (s *Service) Check(ctx context.Context, request string) (*CheckResult, error) {
	p1, err := s.Assist(ctx, request)
	if err != nil {
		return nil, err
	}
	img1, err := s.Art.Generate(ctx, p1)
	if err != nil {
		return nil, err
	}
	res := &CheckResult{Image: img1, Prompt: p1, Drafts: []string{p1}}

	p2, err := s.Critique(ctx, request, p1)
	if err != nil {
		return res, err // the first image still exists
	}
	if p2 == p1 {
		return res, nil // the mind saw nothing to fix
	}
	img2, err := s.Art.Generate(ctx, p2)
	if err != nil {
		return res, err // the first image still exists
	}
	res.Image, res.Prompt = img2, p2
	res.Drafts = append(res.Drafts, p2)
	return res, nil
}

// Critique asks the thinking mind to judge a prompt against the
// original request and return an improved one.
func (s *Service) Critique(ctx context.Context, request, prompt string) (string, error) {
	user := "Original request:\n" + request + "\n\nPrompt used:\n" + prompt
	out, err := s.Brain.Chat(s.Model, []models.ChatMessage{
		{Role: "system", Content: checkSystem},
		{Role: "user", Content: user},
	})
	if err != nil {
		return "", fmt.Errorf("photomind: critique: %w", err)
	}
	return cleanPrompt(out)
}

// labelREs match a "Prompt:" / "Improved prompt:" style label at the
// very start of the brain's reply, inline or on its own line.
var (
	labelPlainRE = regexp.MustCompile(`(?i)^[ \t\n]*prompt[ \t-]*:[ \t\n]*`)
	labelNamedRE = regexp.MustCompile(`(?i)^[ \t\n]*(improved|revised|final|new|updated|suggested|better)[ \t-]*prompt[ \t-]*:[ \t\n]*`)
)

// cleanPrompt normalizes the brain's reply into a bare prompt: it strips
// code fences, keeps only the first paragraph, drops a leading
// "Prompt:" label and surrounding quotes, and caps the length.
func cleanPrompt(s string) (string, error) {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "```") {
		t = strings.TrimPrefix(t, "```")
		// A fence may open with a language tag on its own line
		// (```prompt); a real prompt line always has a space in it.
		if j := strings.IndexByte(t, '\n'); j >= 0 {
			if tag := strings.TrimSpace(t[:j]); tag != "" && !strings.Contains(tag, " ") {
				t = t[j+1:]
			}
		}
		if i := strings.LastIndex(t, "```"); i >= 0 {
			t = t[:i]
		}
		t = strings.TrimSpace(t)
	}
	if i := strings.Index(t, "\n\n"); i >= 0 {
		t = t[:i]
	}
	t = labelNamedRE.ReplaceAllString(t, "")
	t = labelPlainRE.ReplaceAllString(t, "")
	t = strings.Trim(t, "\"'")
	t = strings.TrimSpace(t)
	if t == "" {
		return "", errors.New("the thinking mind returned an empty prompt")
	}
	if r := []rune(t); len(r) > 1500 {
		t = string(r[:1500])
	}
	return t, nil
}
