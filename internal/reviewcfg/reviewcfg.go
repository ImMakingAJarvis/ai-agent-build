// Package reviewcfg loads and validates the review queues config
// (config/review.yaml). The PR review queues build (Job 1 of
// docs/pr-review-build-plan.md) is the first consumer; the merge gate
// (Job 8) reads merge_allow and the caps from here.
package reviewcfg

import (
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Reviewer is one entry under reviewers:.
type Reviewer struct {
	Queue        string `yaml:"queue"`
	Kind         string `yaml:"kind"`
	NeedsRelease bool   `yaml:"needs_release"`
}

// Config is the root of config/review.yaml.
type Config struct {
	MonthCapUSD       float64             `yaml:"month_cap_usd"`
	MaxOpenPRs        int                 `yaml:"max_open_prs"`
	MaxRounds         int                 `yaml:"max_rounds"`
	MaxOpenPoints     int                 `yaml:"max_open_points"`
	MergeAllow        []string            `yaml:"merge_allow"`
	AuthorJobClass    string              `yaml:"author_job_class"`
	AuthorJobPriority int                 `yaml:"author_job_priority"`
	PricesUSDPerMTok  map[string]float64  `yaml:"prices_usd_per_mtok"`
	Reviewers         map[string]Reviewer `yaml:"reviewers"`
	SensitivePaths    []string            `yaml:"sensitive_paths"`
}

// Load reads and validates the review config at path.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(data)
}

// Parse decodes and validates the review config bytes.
func Parse(data []byte) (Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, err
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() error {
	if c.MonthCapUSD <= 0 {
		return fmt.Errorf("month_cap_usd: must be greater than 0, got %v", c.MonthCapUSD)
	}
	if c.MaxOpenPRs < 1 {
		return fmt.Errorf("max_open_prs: must be at least 1, got %d", c.MaxOpenPRs)
	}
	if c.MaxRounds < 1 {
		return fmt.Errorf("max_rounds: must be at least 1, got %d", c.MaxRounds)
	}
	if c.MaxOpenPoints < 1 {
		return fmt.Errorf("max_open_points: must be at least 1, got %d", c.MaxOpenPoints)
	}
	queueOwner := make(map[string]string, len(c.Reviewers))
	for id, r := range c.Reviewers {
		if strings.TrimSpace(r.Queue) == "" {
			return fmt.Errorf("reviewers.%s.queue: must not be empty", id)
		}
		if owner, dup := queueOwner[r.Queue]; dup {
			return fmt.Errorf("reviewers.%s.queue: queue %q is already used by %s", id, r.Queue, owner)
		}
		queueOwner[r.Queue] = id
	}
	if c.AuthorJobClass != "build" {
		return fmt.Errorf("author_job_class: must be build, got %q", c.AuthorJobClass)
	}
	for i, p := range c.SensitivePaths {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("sensitive_paths[%d]: must not be empty", i)
		}
	}
	return nil
}
