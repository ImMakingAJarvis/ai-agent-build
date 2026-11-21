// Package tmdb fetches media metadata from the TMDB v3 API and serves
// repeated lookups from a small on-disk cache, so rescans of an unchanged
// library make zero network calls. The cache directory lives next to the
// database (the NAS media mount is read-only): metadata JSON under
// <dir>/meta, artwork images under <dir>/artwork.
package tmdb

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// Cache is a keyed on-disk store: one JSON blob per meta key under
// <Dir>/meta, one image per artwork file name under <Dir>/artwork.
// Writes are atomic (temp file + rename) so a crash cannot leave a
// half-written entry behind.
type Cache struct {
	Dir string
}

// NewCache prepares the cache directories and returns the cache.
func NewCache(dir string) (*Cache, error) {
	if err := os.MkdirAll(filepath.Join(dir, "meta"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "artwork"), 0o755); err != nil {
		return nil, err
	}
	return &Cache{Dir: dir}, nil
}

// Key turns a logical cache key ("movie|Big Buck Bunny|2008") into a
// stable file-name hash.
func Key(parts ...string) string {
	h := sha1.Sum([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])
}

// Get returns the cached meta blob for key (nil, false when absent or
// unreadable — a corrupt entry is refetched, not served).
func (c *Cache) Get(key string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(c.Dir, "meta", key+".json"))
	if err != nil {
		return nil, false
	}
	return b, true
}

// Put stores a meta blob atomically.
func (c *Cache) Put(key string, data []byte) error {
	return atomicWrite(filepath.Join(c.Dir, "meta", key+".json"), data)
}

// Artwork returns the on-disk path of a cached image file name ("" when
// the file is not there).
func (c *Cache) Artwork(fileName string) string {
	p := filepath.Join(c.Dir, "artwork", fileName)
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// SaveArtwork stores an image atomically under its file name.
func (c *Cache) SaveArtwork(fileName string, data []byte) error {
	return atomicWrite(filepath.Join(c.Dir, "artwork", fileName), data)
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	return os.Rename(name, path)
}
