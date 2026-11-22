package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBuildBodyRaw(t *testing.T) {
	b, err := buildBody("hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("got %q", b)
	}
}

func TestBuildBodyPayload(t *testing.T) {
	b, err := buildBody("", []string{"kind=scan", "count=3"})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]string
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if obj["kind"] != "scan" || obj["count"] != "3" {
		t.Fatalf("got %v", obj)
	}
}

func TestBuildBodyPayloadWins(t *testing.T) {
	b, err := buildBody("raw", []string{"a=b"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"a":"b"}` {
		t.Fatalf("payload should win, got %s", b)
	}
}

func TestBuildBodyBadPayload(t *testing.T) {
	if _, err := buildBody("", []string{"noequals"}); err == nil {
		t.Fatal("want error for k=v without =")
	}
	if _, err := buildBody("", []string{"=v"}); err == nil {
		t.Fatal("want error for empty key")
	}
}

func TestBuildBodyEmpty(t *testing.T) {
	if _, err := buildBody("", nil); err == nil {
		t.Fatal("want error with no body and no payload")
	}
}

func TestParseSince(t *testing.T) {
	t0 := time.Now()
	got, err := parseSince("2h")
	if err != nil {
		t.Fatal(err)
	}
	if d := t0.Sub(got); d < time.Hour || d > 3*time.Hour {
		t.Fatalf("2h ago gave %v (delta %v)", got, d)
	}
	want := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	got, err = parseSince("2026-11-21T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, err := parseSince("yesterday"); err == nil {
		t.Fatal("want error for unparseable input")
	}
}

func TestSplitPos(t *testing.T) {
	topic, rest, ok := splitPos([]string{"voice", "-n", "3"})
	if !ok || topic != "voice" || len(rest) != 2 || rest[0] != "-n" || rest[1] != "3" {
		t.Fatalf("got %q %v %v", topic, rest, ok)
	}
	topic, rest, ok = splitPos([]string{"-n", "3", "voice"})
	if !ok || topic != "voice" || len(rest) != 2 {
		t.Fatalf("got %q %v %v", topic, rest, ok)
	}
	if _, _, ok := splitPos(nil); ok {
		t.Fatal("want no positional for empty args")
	}
	if _, _, ok := splitPos([]string{"-n", "3"}); ok {
		t.Fatal("want no positional for flags only")
	}
}

func TestClip(t *testing.T) {
	if clip("short", 10) != "short" {
		t.Fatal("short strings pass through")
	}
	if got := clip("0123456789", 4); got != "0123…" { // 4 + ellipsis
		t.Fatalf("got %q", got)
	}
}
