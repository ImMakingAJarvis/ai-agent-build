// Package main is capworker-remote: the single-binary packaging of the
// capability worker for a second machine (EVB-37, plan §3.9). One static
// Go binary + one YAML file starts N capability workers on the node,
// each bound to its cap.<name> queue with location remote:<node>
// (consumer priority 1 — a slow link must not cut in front of the box's
// local workers), heartbeats on the events exchange, and self-reporting
// of its own free RAM/VRAM to resourced (read-only lease accounting:
// the remote never takes leases from the box).
//
// Config layout (capworker-remote.yaml):
//
//	node: node2                    # node id: user is remote-<node>,
//	                               # location/device is remote:<node>
//	broker:
//	  url: amqp://remote-node2:secret@111.1.2.222:5672/
//	  # off-LAN only: url: amqps://remote-node2:secret@111.1.2.222:5671/
//	  # tls:
//	  #   ca_file: /etc/jarvis/ca.pem
//	resourced:
//	  url: http://111.1.2.222:9104  # self-report target; empty = off
//	artifacts:
//	  dir: /mnt/nas/jarvis/artifacts
//	heartbeat: 30s
//	self_report: 60s
//	capabilities:
//	  - name: echo                 # queue cap.<name>
//	    builtin: echo
//	    delay: 10s
//	  - name: transcribe
//	    command: [/usr/local/bin/transcribe.sh]
//	    env: [API_URL=...]
//	    timeout: 10m
//
// The broker user is least-privilege (rabbitmq/remote-user.sh): it can
// consume cap.<its caps> and publish flow.* + events, and nothing else.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

// Config is one node's capworker-remote.yaml.
type Config struct {
	// Node is the node id: the worker location/device is remote:<node>,
	// the RabbitMQ user is remote-<node>. Required.
	Node string `yaml:"node"`
	// Broker is the broker connection (URL + TLS trust).
	Broker Broker `yaml:"broker"`
	// Resourced is the self-report target; empty URL = no self-report.
	Resourced Resourced `yaml:"resourced"`
	// Artifacts is where request artifacts (and idempotency markers)
	// go: <dir>/cap-<cap>/<corr>/. Default ~/.local/share/jarvis/artifacts.
	Artifacts Artifacts `yaml:"artifacts"`
	// Heartbeat is the worker.<node>.heartbeat interval on the events
	// exchange. Default 30s.
	Heartbeat duration `yaml:"heartbeat"`
	// SelfReport is the POST /device-report interval (only used when
	// Resourced.URL is set). Default 60s.
	SelfReport duration `yaml:"self_report"`
	// Capabilities are the capabilities this node serves (≥1).
	Capabilities []Capability `yaml:"capabilities"`
}

// Broker is the broker connection.
type Broker struct {
	// URL is the broker URL (amqp:// inside the LAN, amqps:// off-LAN).
	URL string `yaml:"url"`
	// TLS tunes the amqps:// trust.
	TLS TLS `yaml:"tls"`
}

// TLS is the broker's TLS trust (off-LAN links, plan §3.9).
type TLS struct {
	// CAFile is a PEM CA bundle the broker's certificate chains to
	// (the CA both nodes share). Empty = system roots.
	CAFile string `yaml:"ca_file"`
	// InsecureSkipVerify disables certificate verification (TLS still
	// encrypts the link). LAN/lab only.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

// Resourced is the self-report target.
type Resourced struct {
	// URL is the resourced base URL (e.g. http://111.1.2.222:9104).
	// Empty = no self-report.
	URL string `yaml:"url"`
}

// Artifacts is the artifact root.
type Artifacts struct {
	// Dir is the root; per-request artifacts land in
	// <dir>/cap-<cap>/<corr>/.
	Dir string `yaml:"dir"`
}

// Capability is one capability this node serves.
type Capability struct {
	// Name is the capability (queue cap.<name>). Required.
	Name string `yaml:"name"`
	// Builtin selects a built-in handler: "echo" (the test
	// capability, same contract as cmd/cap-echo). Exactly one of
	// Builtin and Command must be set.
	Builtin string `yaml:"builtin"`
	// Command runs one external program per request (see
	// commandHandler for the stdin/stdout protocol).
	Command []string `yaml:"command"`
	// Env are extra KEY=V pairs for a command handler (on top of the
	// JARVIS_* request variables).
	Env []string `yaml:"env"`
	// Delay is the echo handler's delay. Default 30s.
	Delay duration `yaml:"delay"`
	// Timeout bounds one command run. Default 10m.
	Timeout duration `yaml:"timeout"`
}

// duration is a YAML time.Duration: yaml.v3 has no duration scalar, so
// the config parses "30s" through this. A bare integer is read as
// nanoseconds (a Go duration's natural unit).
type duration time.Duration

func (d *duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		if v, err := time.ParseDuration(node.Value); err == nil {
			*d = duration(v)
			return nil
		}
		if v, err := strconv.ParseInt(node.Value, 10, 64); err == nil {
			*d = duration(time.Duration(v) * time.Nanosecond)
			return nil
		}
	}
	return fmt.Errorf("bad duration %q (want e.g. \"30s\")", node.Value)
}

// Load reads and validates one node config, filling defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("capworker-remote: read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("capworker-remote: parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Artifacts.Dir == "" {
		c.Artifacts.Dir = filepath.Join(os.Getenv("HOME"), ".local", "share", "jarvis", "artifacts")
	}
	if c.Heartbeat <= 0 {
		c.Heartbeat = duration(30 * time.Second)
	}
	if c.Resourced.URL != "" && c.SelfReport <= 0 {
		c.SelfReport = duration(60 * time.Second)
	}
	for i := range c.Capabilities {
		cap := &c.Capabilities[i]
		if cap.Delay <= 0 {
			cap.Delay = duration(30 * time.Second)
		}
		if len(cap.Command) > 0 && cap.Timeout <= 0 {
			cap.Timeout = duration(10 * time.Minute)
		}
	}
}

var idRe = regexp.MustCompile(`^[a-z0-9-]+$`)

func (c *Config) validate() error {
	if !idRe.MatchString(c.Node) {
		return fmt.Errorf("capworker-remote: node must match [a-z0-9-]+ (got %q)", c.Node)
	}
	if c.Broker.URL == "" {
		return fmt.Errorf("capworker-remote: broker.url is required")
	}
	if len(c.Capabilities) == 0 {
		return fmt.Errorf("capworker-remote: at least one capability is required")
	}
	seen := map[string]bool{}
	for i, cap := range c.Capabilities {
		if !idRe.MatchString(cap.Name) {
			return fmt.Errorf("capworker-remote: capabilities[%d].name must match [a-z0-9-]+ (got %q)", i, cap.Name)
		}
		if seen[cap.Name] {
			return fmt.Errorf("capworker-remote: duplicate capability %q", cap.Name)
		}
		seen[cap.Name] = true
		hasBuiltin := cap.Builtin == "echo"
		hasCommand := len(cap.Command) > 0
		switch {
		case hasBuiltin && hasCommand:
			return fmt.Errorf("capworker-remote: capabilities[%d] %q: set exactly one of builtin or command (both set)", i, cap.Name)
		case !hasBuiltin && cap.Builtin != "":
			return fmt.Errorf("capworker-remote: capabilities[%d]: unsupported builtin %q (only \"echo\")", i, cap.Builtin)
		case !hasBuiltin && !hasCommand:
			return fmt.Errorf("capworker-remote: capabilities[%d] %q: set exactly one of builtin or command", i, cap.Name)
		case !hasBuiltin && cap.Command[0] == "":
			return fmt.Errorf("capworker-remote: capabilities[%d] %q: command[0] (the program) is empty", i, cap.Name)
		}
	}
	return nil
}

// dialOpts turns the broker config into queue dial options, reading the
// CA bundle when one is configured.
func (b Broker) dialOpts() (queue.DialOpts, error) {
	o := queue.DialOpts{URL: b.URL, InsecureSkipVerify: b.TLS.InsecureSkipVerify}
	if b.TLS.CAFile != "" {
		ca, err := os.ReadFile(b.TLS.CAFile)
		if err != nil {
			return queue.DialOpts{}, fmt.Errorf("capworker-remote: read ca_file %s: %w", b.TLS.CAFile, err)
		}
		o.CACert = ca
	}
	return o, nil
}
