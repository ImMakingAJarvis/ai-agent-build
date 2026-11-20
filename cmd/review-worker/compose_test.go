package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The review containers must stay CPU-only (Job 12a of
// docs/pr-review-build-plan.md): the reviewers run the claude CLI against the
// Anthropic API, so a GPU reservation in their service block would let a
// reviewer load the 27B and wedge the 4090. These tests fail if any of the
// four review service blocks in docker-compose.yml carries a GPU.

// reviewServices are the compose service names that must never carry a GPU.
var reviewServices = []string{"review-sonnet", "review-opus", "review-fable", "review-sweep"}

// composeServiceBlocks parses the services map of a compose file. It returns
// the raw block of each service so a scanner can inspect it.
func composeServiceBlocks(content string) (map[string]any, error) {
	var doc struct {
		Services map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, err
	}
	return doc.Services, nil
}

// gpuInService reports whether a parsed service block carries a GPU: a
// "devices" key at any depth (deploy.resources.reservations.devices) or
// "nvidia" in any key or string value (runtime: nvidia,
// NVIDIA_VISIBLE_DEVICES, an nvidia-smi mount). A devices: entry under a
// different service block is never seen here — the scanner only walks the
// block it is given.
func gpuInService(v any) bool {
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			if strings.EqualFold(k, "devices") || strings.Contains(strings.ToLower(k), "nvidia") {
				return true
			}
			if gpuInService(child) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if gpuInService(child) {
				return true
			}
		}
	case string:
		return strings.Contains(strings.ToLower(node), "nvidia")
	}
	return false
}

// composeGPUFindings maps every service name in the file to whether its
// block carries a GPU.
func composeGPUFindings(content string) (map[string]bool, error) {
	services, err := composeServiceBlocks(content)
	if err != nil {
		return nil, err
	}
	findings := map[string]bool{}
	for name, block := range services {
		findings[name] = gpuInService(block)
	}
	return findings, nil
}

// TestComposeServiceScanner tests the scanner on a fixture, not on the real
// file: two services, one with a GPU and one without. Only the GPU one must
// be flagged — a devices: entry under the other service must not leak into
// its result.
func TestComposeServiceScanner(t *testing.T) {
	fixture := `
services:
  gpu-helper:
    image: example/gpu:local
    runtime: nvidia
    environment:
      NVIDIA_VISIBLE_DEVICES: "all"
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: 1
  plain-helper:
    image: example/plain:local
    environment:
      HOME: /home/agent
      RABBITMQ_URL: amqp://jarvis:jarvis-local@rabbitmq:5672
    volumes:
      - /home/agent/.config/jarvis:/home/agent/.config/jarvis
`
	findings, err := composeGPUFindings(fixture)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if !findings["gpu-helper"] {
		t.Error("gpu-helper not flagged: the GPU service must fail")
	}
	if findings["plain-helper"] {
		t.Error("plain-helper flagged: the devices: of the other service must not fail it")
	}
}

// repoRootCompose walks up from the package dir to the docker-compose.yml at
// the repo root (the test runs from cmd/review-worker).
func repoRootCompose(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		cand := filepath.Join(dir, "docker-compose.yml")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("docker-compose.yml not found above the package dir")
		}
		dir = parent
	}
}

// TestComposeReviewNoGPU reads the real docker-compose.yml and fails if any
// of the four review service blocks appears in the same block as devices: or
// nvidia. It also fails if a review service is missing from the file, so a
// rename or deletion cannot quietly drop the guarantee.
func TestComposeReviewNoGPU(t *testing.T) {
	path := repoRootCompose(t)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	services, err := composeServiceBlocks(string(content))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	findings, err := composeGPUFindings(string(content))
	if err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	for _, name := range reviewServices {
		if _, ok := services[name]; !ok {
			t.Errorf("service %s missing from docker-compose.yml", name)
		}
		if findings[name] {
			t.Errorf("service %s carries a GPU (devices: or nvidia in its block)", name)
		}
	}
}
