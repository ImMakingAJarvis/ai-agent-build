package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

// newFullAutoRedis points JARVIS_REDIS at a fresh miniredis and returns
// the server (for corrupting values directly).
func newFullAutoRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	mr := miniredis.RunT(t)
	t.Cleanup(func() { mr.Close() })
	t.Setenv("JARVIS_REDIS", "redis://"+mr.Addr())
	return mr
}

func TestCmdFullAutoGetMissingKeyIsOff(t *testing.T) {
	newFullAutoRedis(t)
	var out bytes.Buffer
	if err := cmdFullAuto(&out, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "off\n" {
		t.Fatalf("get = %q, want off", got)
	}
}

func TestCmdFullAutoSetGetRoundtrip(t *testing.T) {
	newFullAutoRedis(t)
	var out bytes.Buffer
	if err := cmdFullAuto(&out, []string{"on"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "on\n" {
		t.Fatalf("set on = %q", got)
	}
	out.Reset()
	if err := cmdFullAuto(&out, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "on\n" {
		t.Fatalf("get after on = %q", got)
	}
	out.Reset()
	if err := cmdFullAuto(&out, []string{"OFF"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "off\n" {
		t.Fatalf("set off = %q", got)
	}
}

func TestCmdFullAutoCorruptValueIsOff(t *testing.T) {
	mr := newFullAutoRedis(t)
	mr.Set("nobrakes:full_auto", "banana")
	var out bytes.Buffer
	if err := cmdFullAuto(&out, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "off\n" {
		t.Fatalf("get with corrupt value = %q, want off", got)
	}
}

func TestCmdFullAutoRedisDisabled(t *testing.T) {
	t.Setenv("JARVIS_REDIS", "off")
	var out bytes.Buffer
	// A get fails safe to off.
	if err := cmdFullAuto(&out, nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "off\n" {
		t.Fatalf("get with JARVIS_REDIS=off = %q, want off", got)
	}
	// A set must not pretend it worked.
	out.Reset()
	if err := cmdFullAuto(&out, []string{"on"}); err == nil {
		t.Fatal("set with JARVIS_REDIS=off: got nil error, want an error")
	}
}

func TestCmdFullAutoUnreachable(t *testing.T) {
	t.Setenv("JARVIS_REDIS", "redis://127.0.0.1:1")
	var out bytes.Buffer
	// A get fails safe to off: the loop must never stall on a dead Redis.
	if err := cmdFullAuto(&out, nil); err != nil {
		t.Fatalf("get with unreachable redis: %v", err)
	}
	if got := out.String(); got != "off\n" {
		t.Fatalf("get with unreachable redis = %q, want off", got)
	}
	// A set reports the failure to the operator.
	out.Reset()
	err := cmdFullAuto(&out, []string{"on"})
	if err == nil {
		t.Fatal("set with unreachable redis: got nil error, want an error")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("set error = %v, want it to mention the unreachable redis", err)
	}
}
