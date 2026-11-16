package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = `
node: node2
broker:
  url: amqp://remote-node2:secret@10.0.0.5:5672/
resourced:
  url: http://10.0.0.5:9104
artifacts:
  dir: /mnt/nas/jarvis/artifacts
heartbeat: 15s
capabilities:
  - name: echo
    builtin: echo
    delay: 10s
  - name: transcribe
    command: [/usr/local/bin/transcribe.sh, --fast]
    env: [API_URL=http://10.0.0.5:8000]
    timeout: 5m
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capworker-remote.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Node != "node2" || cfg.Broker.URL != "amqp://remote-node2:secret@10.0.0.5:5672/" {
		t.Errorf("identity = (%q %q)", cfg.Node, cfg.Broker.URL)
	}
	if time.Duration(cfg.Heartbeat) != 15*time.Second {
		t.Errorf("heartbeat = %s, want 15s", time.Duration(cfg.Heartbeat))
	}
	if time.Duration(cfg.SelfReport) != 60*time.Second {
		t.Errorf("self_report default = %s, want 60s (resourced.url set)", time.Duration(cfg.SelfReport))
	}
	if len(cfg.Capabilities) != 2 {
		t.Fatalf("capabilities = %d, want 2", len(cfg.Capabilities))
	}
	echo, cmd := cfg.Capabilities[0], cfg.Capabilities[1]
	if echo.Builtin != "echo" || time.Duration(echo.Delay) != 10*time.Second {
		t.Errorf("echo cap = %+v", echo)
	}
	if len(cmd.Command) != 2 || cmd.Command[0] != "/usr/local/bin/transcribe.sh" {
		t.Errorf("command cap = %+v", cmd)
	}
	if time.Duration(cmd.Timeout) != 5*time.Minute {
		t.Errorf("command timeout = %s, want 5m", time.Duration(cmd.Timeout))
	}
}

func TestLoadDefaults(t *testing.T) {
	body := `
node: n
broker:
  url: amqp://u:p@h/
capabilities:
  - name: echo
    builtin: echo
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if time.Duration(cfg.Heartbeat) != 30*time.Second {
		t.Errorf("heartbeat default = %s, want 30s", time.Duration(cfg.Heartbeat))
	}
	if time.Duration(cfg.SelfReport) != 0 {
		t.Errorf("self_report with no resourced.url = %s, want 0 (off)", time.Duration(cfg.SelfReport))
	}
	if time.Duration(cfg.Capabilities[0].Delay) != 30*time.Second {
		t.Errorf("echo delay default = %s, want 30s", time.Duration(cfg.Capabilities[0].Delay))
	}
}

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"missing node", "broker:\n  url: amqp://u:p@h/\ncapabilities:\n  - name: echo\n    builtin: echo\n", "node"},
		{"bad node id", "node: Node_2\nbroker:\n  url: amqp://u:p@h/\ncapabilities:\n  - name: echo\n    builtin: echo\n", "node"},
		{"missing broker url", "node: n\ncapabilities:\n  - name: echo\n    builtin: echo\n", "broker.url"},
		{"no capabilities", "node: n\nbroker:\n  url: amqp://u:p@h/\n", "capability"},
		{"builtin and command", "node: n\nbroker:\n  url: amqp://u:p@h/\ncapabilities:\n  - name: x\n    builtin: echo\n    command: [/bin/true]\n", "exactly one"},
		{"neither builtin nor command", "node: n\nbroker:\n  url: amqp://u:p@h/\ncapabilities:\n  - name: x\n", "exactly one"},
		{"unknown builtin", "node: n\nbroker:\n  url: amqp://u:p@h/\ncapabilities:\n  - name: x\n    builtin: transcribe\n", "unsupported builtin"},
		{"empty command program", "node: n\nbroker:\n  url: amqp://u:p@h/\ncapabilities:\n  - name: x\n    command: [\"\"]\n", "program"},
		{"duplicate capability", "node: n\nbroker:\n  url: amqp://u:p@h/\ncapabilities:\n  - name: x\n    builtin: echo\n  - name: x\n    builtin: echo\n", "duplicate"},
		{"bad duration", "node: n\nbroker:\n  url: amqp://u:p@h/\nheartbeat: soon\ncapabilities:\n  - name: echo\n    builtin: echo\n", "duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.body))
			if err == nil {
				t.Fatalf("Load succeeded, want an error mentioning %q", tt.want)
			}
			if !contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadBareIntDuration(t *testing.T) {
	body := "node: n\nbroker:\n  url: amqp://u:p@h/\nheartbeat: 1000000000\ncapabilities:\n  - name: echo\n    builtin: echo\n"
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if time.Duration(cfg.Heartbeat) != time.Second {
		t.Errorf("bare-int heartbeat = %s, want 1s (nanoseconds)", time.Duration(cfg.Heartbeat))
	}
}

func TestLoadUnreadableCA(t *testing.T) {
	body := "node: n\nbroker:\n  url: amqps://u:p@h:5671/\n  tls:\n    ca_file: /nonexistent/ca.pem\ncapabilities:\n  - name: echo\n    builtin: echo\n"
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load (the file read happens at dial time, not load time): %v", err)
	}
	if _, err := cfg.Broker.dialOpts(); err == nil {
		t.Fatal("dialOpts with a missing ca_file must fail")
	}
}

func TestDialOptsWithCA(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, []byte("PEM-DATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := Broker{URL: "amqps://u:p@h:5671/", TLS: TLS{CAFile: ca, InsecureSkipVerify: true}}
	o, err := b.dialOpts()
	if err != nil {
		t.Fatalf("dialOpts: %v", err)
	}
	if string(o.CACert) != "PEM-DATA" || !o.InsecureSkipVerify || o.URL != b.URL {
		t.Errorf("dialOpts = %+v", o)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
