// Package secrets stores write-only secrets on disk: one file per secret,
// mode 0600, under a 0700 directory. The dashboard exposes add, remove and
// list-by-name only — no endpoint ever returns a value. The local
// jarvis-secrets CLI (scripts/jarvis-secrets) is the reader the agent uses.
package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// MaxValueLen bounds a single secret (1 MiB covers keys, tokens and pasted
// credentials; it keeps a bad paste from filling the disk).
const MaxValueLen = 1 << 20

// nameRe keeps names file-safe: no separators (no path traversal), no
// leading dot (no hidden files, no "." / ".."), 1-128 chars.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateName reports whether name is a legal secret name.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("bad secret name %q (letters, digits, '.', '_' and '-', starting with a letter or digit)", name)
	}
	return nil
}

// Info describes one stored secret. It deliberately has no value field:
// nothing on the dashboard API path may ever carry the value.
type Info struct {
	Name    string    `json:"name"`
	Bytes   int       `json:"bytes"`
	Updated time.Time `json:"updated"`
}

// Store is a directory of secret files.
type Store struct {
	mu   sync.Mutex
	root string
}

// Open creates (if needed) the secrets subdirectory of dir and returns a
// store. The directory is 0700 so only the owning user can even list it.
func Open(dir string) (*Store, error) {
	root := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("secrets: mkdir %s: %w", root, err)
	}
	// umask can mask MkdirAll's mode; enforce the intended one.
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secrets: chmod %s: %w", root, err)
	}
	return &Store{root: root}, nil
}

// Root returns the directory the store holds its files in.
func (s *Store) Root() string { return s.root }

// Put writes (or overwrites) the secret named name with value.
func (s *Store) Put(name, value string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if len(value) > MaxValueLen {
		return fmt.Errorf("secret value too large (%d bytes, max %d)", len(value), MaxValueLen)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeAtomic(filepath.Join(s.root, name), []byte(value))
}

// Get returns the value of the secret named name. Only the local CLI uses
// this; the dashboard never calls it.
func (s *Store) Get(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(s.root, name))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no secret named %q", name)
		}
		return "", err
	}
	return string(b), nil
}

// Delete removes the secret named name. A missing name is an error.
func (s *Store) Delete(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(filepath.Join(s.root, name))
	if os.IsNotExist(err) {
		return fmt.Errorf("no secret named %q", name)
	}
	return err
}

// List returns metadata for every stored secret, sorted by name. Values are
// never read or returned; files that are not valid secret names (leftover
// temp files, foreign files) are skipped, never exposed.
func (s *Store) List() ([]Info, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !nameRe.MatchString(name) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Info{Name: name, Bytes: int(fi.Size()), Updated: fi.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// writeAtomic writes b to path as a 0600 regular file: write to a hidden
// temp file in the same directory (hidden so List skips a crashed temp),
// chmod, then rename over the target.
func writeAtomic(path string, b []byte) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil { // umask may have masked WriteFile's mode
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
