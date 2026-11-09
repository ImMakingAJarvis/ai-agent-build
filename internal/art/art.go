// Package art is a small client for the local photo-generation service
// (scripts/art_service.py, Z-Image-Turbo behind FastAPI, port 8120).
// The Discord daemon uses it to turn "!photo <prompt>" into an image.
package art

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// DefaultBase is where the art service listens for the local API clients
// (they run on this host, so loopback is the right address for calls).
const DefaultBase = "http://127.0.0.1:8120"

// DefaultGallery is the LAN address the owner opens in a browser — this
// box's LAN IP. Owner-facing links must never be loopback: from a computer
// on the network, localhost points at that computer, not the jarvis box.
const DefaultGallery = "http://111.1.2.27:8120"

// Client talks to one art service.
type Client struct {
	base string
	http *http.Client
}

// Image is the result of one generation.
type Image struct {
	Path    string  `json:"path"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	Seed    int64   `json:"seed"`
	Steps   int     `json:"steps"`
	Seconds float64 `json:"seconds"`
}

// ErrBusy means a generation is already running (HTTP 409).
type ErrBusy struct{ Detail string }

func (e *ErrBusy) Error() string { return "art: a generation is already running" + detail(e.Detail) }

// ErrUnavailable means the service or the model could not generate
// (HTTP 503, unreachable, or any other failure).
type ErrUnavailable struct{ Detail string }

func (e *ErrUnavailable) Error() string { return "art: " + or(e.Detail, "generation failed") }

// New builds a client from ART_URL, defaulting to the host listener.
func New() *Client {
	u := os.Getenv("ART_URL")
	if u == "" {
		u = DefaultBase
	}
	return &Client{base: strings.TrimRight(u, "/"), http: &http.Client{}}
}

// NewWithURL builds a client for an explicit base URL (tests).
func NewWithURL(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{}}
}

// Generate runs one photo generation and blocks until it finishes.
// size is the requested size — "1024x576" (explicit pixels) or an aspect
// like "16x9" (long side scaled to the service's max) — and may be "" for
// the service default (square 1024x1024). An image takes tens of seconds
// on the 4090; give the context a few minutes.
func (c *Client) Generate(ctx context.Context, prompt, size string) (*Image, error) {
	reqBody := map[string]string{"prompt": prompt}
	if size != "" {
		reqBody["size"] = size
	}
	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &ErrUnavailable{Detail: "cannot reach the art service: " + err.Error()}
	}
	defer res.Body.Close()

	detail, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	switch {
	case res.StatusCode == http.StatusOK:
		var img Image
		if err := json.Unmarshal(detail, &img); err != nil {
			return nil, &ErrUnavailable{Detail: "art service returned bad JSON: " + err.Error()}
		}
		return &img, nil
	case res.StatusCode == http.StatusConflict:
		return nil, &ErrBusy{Detail: strings.TrimSpace(string(detail))}
	case res.StatusCode == http.StatusServiceUnavailable:
		return nil, &ErrUnavailable{Detail: strings.TrimSpace(string(detail))}
	default:
		return nil, &ErrUnavailable{Detail: fmt.Sprintf("art service error %d: %s", res.StatusCode, strings.TrimSpace(string(detail)))}
	}
}

// GalleryURL returns the URL the owner can open in a browser to see every
// generated photo — the LAN address by default, so the link works from
// any computer on the network (loopback would not). ART_GALLERY_URL
// overrides the default.
func GalleryURL() string {
	if u := os.Getenv("ART_GALLERY_URL"); u != "" {
		return u
	}
	return DefaultGallery
}

// sizeRE matches a size token: "1024x576" (pixels) or "16x9" (aspect),
// with 'x' or ':' as the separator.
var sizeRE = regexp.MustCompile(`^(\d{1,4})\s*[x:]\s*(\d{1,4})$`)

// ParseSize parses a size token such as "1024x576", "16x9" or "16:9".
// It returns the two numbers as written; the art service turns small
// numbers (<= 128) into an aspect ratio — long side scaled to its max —
// and larger ones into explicit pixels. ok is false for anything else
// (so "sunset", "1024" or "0x0" are not size tokens).
func ParseSize(token string) (w, h int, ok bool) {
	m := sizeRE.FindStringSubmatch(strings.TrimSpace(token))
	if m == nil {
		return 0, 0, false
	}
	w, _ = strconv.Atoi(m[1])
	h, _ = strconv.Atoi(m[2])
	if w < 1 || h < 1 {
		return 0, 0, false
	}
	return w, h, true
}

func detail(d string) string {
	if d == "" {
		return ""
	}
	return " (" + d + ")"
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
