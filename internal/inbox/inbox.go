// Package inbox holds messages the owner sends to the agent (from the
// queue site or `jarvis-jobs message`). Messages wait in a flat directory
// until the agent's next loop step picks them up: the owner-message hook
// (scripts/owner-message-hook.sh) drains the inbox before each tool call
// and at stop, and the runner folds idle-time messages into the next
// round's prompt. Delivered messages are kept under delivered/ for the
// dashboard's history.
package inbox

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MaxText caps one message in bytes; longer messages are rejected.
const MaxText = 16 * 1024

// keepDelivered caps how many delivered messages stay on disk.
const keepDelivered = 200

// Dir returns the inbox directory: $JARVIS_DATA_DIR/inbox, defaulting to
// ~/.local/share/jarvis/inbox (same root as the agent job store).
func Dir() string {
	base := os.Getenv("JARVIS_DATA_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/home/agent"
		}
		base = filepath.Join(home, ".local", "share", "jarvis")
	}
	return filepath.Join(base, "inbox")
}

// Send stores text as a pending message and returns its file name. Text is
// trimmed; empty or oversized messages are rejected. The file is written
// to a temp name and renamed into place so a half-written message is never
// visible to the hook.
func Send(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("inbox: empty message")
	}
	if len(text) > MaxText {
		return "", fmt.Errorf("inbox: message too long (%d > %d bytes)", len(text), MaxText)
	}
	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%d-%04x.txt", time.Now().UnixNano(), rand.Intn(0x10000))
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return name, nil
}

// Pending lists pending message file names, oldest first (the name starts
// with the UnixNano timestamp, so lexicographic order is time order).
func Pending() ([]string, error) {
	ents, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// Count returns the number of pending messages.
func Count() int {
	p, _ := Pending()
	return len(p)
}

// Drain reads all pending messages (oldest first), moves them to
// delivered/, and returns their combined text. It returns "" when there is
// nothing pending. A message that vanishes between listing and reading is
// skipped and picked up by the next drain.
func Drain() (string, error) {
	pending, err := Pending()
	if err != nil {
		return "", err
	}
	if len(pending) == 0 {
		return "", nil
	}
	dir := Dir()
	delivered := filepath.Join(dir, "delivered")
	var b strings.Builder
	for _, name := range pending {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if err := os.MkdirAll(delivered, 0o755); err != nil {
			return b.String(), err
		}
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(delivered, name)); err != nil {
			return b.String(), err
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.Write(data)
	}
	pruneDelivered(delivered)
	return b.String(), nil
}

// pruneDelivered deletes the oldest delivered messages beyond keepDelivered.
func pruneDelivered(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".txt") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keepDelivered {
		os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}
