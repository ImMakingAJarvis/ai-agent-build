// Package db is the SQLite store. Schema v0: users, libraries, items
// (movies, seasons, episodes linked by parent_id), and scans (the queue-
// driven library scan job: one row per requested scan, with its state and
// the added/updated/skipped counts it produced).
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY,
	email TEXT NOT NULL UNIQUE,
	display_name TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS libraries (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	path TEXT NOT NULL,
	type TEXT NOT NULL DEFAULT 'movies' CHECK (type IN ('movies','shows')),
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS items (
	id INTEGER PRIMARY KEY,
	library_id INTEGER NOT NULL REFERENCES libraries(id),
	type TEXT NOT NULL CHECK (type IN ('movie','season','episode')),
	title TEXT NOT NULL,
	path TEXT NOT NULL,
	parent_id INTEGER REFERENCES items(id),
	year INTEGER NOT NULL DEFAULT 0,
	season INTEGER NOT NULL DEFAULT 0,
	episode INTEGER NOT NULL DEFAULT 0,
	size INTEGER NOT NULL DEFAULT 0,
	modtime TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_items_library ON items(library_id);
CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent_id);
CREATE INDEX IF NOT EXISTS idx_items_path ON items(library_id, path);
CREATE TABLE IF NOT EXISTS scans (
	id INTEGER PRIMARY KEY,
	library_id INTEGER NOT NULL REFERENCES libraries(id),
	state TEXT NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','running','done','error')),
	started_at TEXT,
	finished_at TEXT,
	added INTEGER NOT NULL DEFAULT 0,
	updated INTEGER NOT NULL DEFAULT 0,
	skipped INTEGER NOT NULL DEFAULT 0,
	error TEXT
);
CREATE INDEX IF NOT EXISTS idx_scans_library ON scans(library_id);
`

// migrate brings databases created before the item-metadata columns
// (year/season/episode/size/modtime) up to the current schema. Fresh
// databases get the columns from CREATE TABLE; the ALTERs then hit
// "duplicate column name" and are ignored.
func migrate(d *sql.DB) error {
	for _, c := range []struct{ col, def string }{
		{"year", "INTEGER NOT NULL DEFAULT 0"},
		{"season", "INTEGER NOT NULL DEFAULT 0"},
		{"episode", "INTEGER NOT NULL DEFAULT 0"},
		{"size", "INTEGER NOT NULL DEFAULT 0"},
		{"modtime", "TEXT NOT NULL DEFAULT ''"},
	} {
		_, err := d.Exec("ALTER TABLE items ADD COLUMN " + c.col + " " + c.def)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate %s: %w", c.col, err)
		}
	}
	return nil
}

// DB wraps *sql.DB.
type DB struct{ *sql.DB }

// Library is one media library. JSON names are the spec's field names.
type Library struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
}

// Item is one media item (movie, season or episode). Path is the item's
// location inside its library (a relative path for scanned items); it is
// the scan's upsert key, so (library_id, path) is unique in practice.
type Item struct {
	ID        int64  `json:"id"`
	LibraryID int64  `json:"library_id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Path      string `json:"path"`
	ParentID  *int64 `json:"parent_id"`
	Year      int    `json:"year"`
	Season    int    `json:"season"`
	Episode   int    `json:"episode"`
	Size      int64  `json:"size"`
	ModTime   string `json:"modtime,omitempty"` // RFC3339; "" for folders
}

// ItemFilter narrows ListItems.
type ItemFilter struct {
	LibraryID *int64
	Type      string
	Query     string
	Limit     int
}

// Open opens (creating if needed) the SQLite file and applies the schema.
// The store is touched from several goroutines (the API handlers and the
// scan consumer), so it runs in WAL mode with a busy timeout and a single
// pooled connection: WAL lets reads and writes proceed concurrently, the
// timeout turns lock contention into a wait, and one connection makes the
// database/sql pool serialize writers for us.
func Open(path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	if _, err := d.Exec(schema); err != nil {
		_ = d.Close()
		return nil, err
	}
	if err := migrate(d); err != nil {
		_ = d.Close()
		return nil, err
	}
	return &DB{d}, nil
}

func (d *DB) ListLibraries() ([]Library, error) {
	rows, err := d.Query(`SELECT id, name, path, type FROM libraries ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Library
	for rows.Next() {
		var l Library
		if err := rows.Scan(&l.ID, &l.Name, &l.Path, &l.Type); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// CreateLibrary inserts a library and returns it with its id.
func (d *DB) CreateLibrary(l Library) (Library, error) {
	res, err := d.Exec(`INSERT INTO libraries (name, path, type) VALUES (?, ?, ?)`, l.Name, l.Path, l.Type)
	if err != nil {
		return l, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return l, err
	}
	l.ID = id
	return l, nil
}

func (d *DB) GetLibrary(id int64) (Library, error) {
	var l Library
	err := d.QueryRow(`SELECT id, name, path, type FROM libraries WHERE id = ?`, id).
		Scan(&l.ID, &l.Name, &l.Path, &l.Type)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

// ErrNotFound reports a missing library or item.
var ErrNotFound = errors.New("not found")

func scanItem(rows interface {
	Scan(...any) error
}) (Item, error) {
	var i Item
	if err := rows.Scan(&i.ID, &i.LibraryID, &i.Type, &i.Title, &i.Path, &i.ParentID,
		&i.Year, &i.Season, &i.Episode, &i.Size, &i.ModTime); err != nil {
		return i, err
	}
	return i, nil
}

const itemColumns = "id, library_id, type, title, path, parent_id, year, season, episode, size, modtime"

// InsertItem inserts an item and returns it with its id.
func (d *DB) InsertItem(i Item) (Item, error) {
	res, err := d.Exec(
		`INSERT INTO items (library_id, type, title, path, parent_id, year, season, episode, size, modtime)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.LibraryID, i.Type, i.Title, i.Path, i.ParentID, i.Year, i.Season, i.Episode, i.Size, i.ModTime)
	if err != nil {
		return i, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return i, err
	}
	i.ID = id
	return i, nil
}

// ListItems returns items matching f (all fields optional), ordered by id.
func (d *DB) ListItems(f ItemFilter) ([]Item, error) {
	where := []string{"1=1"}
	var args []any
	if f.LibraryID != nil {
		where = append(where, "library_id = ?")
		args = append(args, *f.LibraryID)
	}
	if f.Type != "" {
		where = append(where, "type = ?")
		args = append(args, f.Type)
	}
	if f.Query != "" {
		where = append(where, "title LIKE ?")
		args = append(args, "%"+f.Query+"%")
	}
	q := `SELECT ` + itemColumns + ` FROM items WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY id`
	if f.Limit > 0 {
		q += " LIMIT " + strconv.Itoa(f.Limit)
	}
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Item
	for rows.Next() {
		i, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (d *DB) GetItem(id int64) (Item, error) {
	row := d.QueryRow(`SELECT `+itemColumns+` FROM items WHERE id = ?`, id)
	i, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("get item: %w", err)
	}
	return i, nil
}

// FindItemByPath returns the item at relPath inside the library.
func (d *DB) FindItemByPath(libraryID int64, path string) (Item, error) {
	row := d.QueryRow(`SELECT `+itemColumns+` FROM items WHERE library_id = ? AND path = ?`, libraryID, path)
	i, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("find item by path: %w", err)
	}
	return i, nil
}

// UpdateItemMeta refreshes an item's scanned metadata (title, size, mtime,
// the parsed numbers, and the parent link for episodes).
func (d *DB) UpdateItemMeta(id int64, i Item) error {
	_, err := d.Exec(
		`UPDATE items SET title = ?, parent_id = ?, year = ?, season = ?, episode = ?, size = ?, modtime = ? WHERE id = ?`,
		i.Title, i.ParentID, i.Year, i.Season, i.Episode, i.Size, i.ModTime, id)
	return err
}

// Scan states. A scan is queued by the API, flipped to running by the
// worker that dequeues it, and ends done or error.
const (
	ScanQueued  = "queued"
	ScanRunning = "running"
	ScanDone    = "done"
	ScanError   = "error"
)

// Scan is one requested library scan and its outcome.
type Scan struct {
	ID         int64  `json:"id"`
	LibraryID  int64  `json:"library_id"`
	State      string `json:"state"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Added      int    `json:"added"`
	Updated    int    `json:"updated"`
	Skipped    int    `json:"skipped"`
	Error      string `json:"error,omitempty"`
}

func scanRow(rows interface{ Scan(...any) error }) (Scan, error) {
	var s Scan
	if err := rows.Scan(&s.ID, &s.LibraryID, &s.State, &s.StartedAt, &s.FinishedAt,
		&s.Added, &s.Updated, &s.Skipped, &s.Error); err != nil {
		return s, err
	}
	return s, nil
}

const scanColumns = "id, library_id, state, COALESCE(started_at,''), COALESCE(finished_at,''), added, updated, skipped, COALESCE(error,'')"

// CreateScan inserts a queued scan for the library.
func (d *DB) CreateScan(libraryID int64) (Scan, error) {
	res, err := d.Exec(`INSERT INTO scans (library_id, state) VALUES (?, ?)`, libraryID, ScanQueued)
	if err != nil {
		return Scan{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Scan{}, err
	}
	return d.GetScan(id)
}

// GetScan returns one scan by id.
func (d *DB) GetScan(id int64) (Scan, error) {
	row := d.QueryRow(`SELECT `+scanColumns+` FROM scans WHERE id = ?`, id)
	s, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Scan{}, ErrNotFound
	}
	return s, err
}

// LatestScan returns the newest scan for the library (or ErrNotFound when
// the library has never been scanned).
func (d *DB) LatestScan(libraryID int64) (Scan, error) {
	row := d.QueryRow(`SELECT `+scanColumns+` FROM scans WHERE library_id = ? ORDER BY id DESC LIMIT 1`, libraryID)
	s, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Scan{}, ErrNotFound
	}
	return s, err
}

// ActiveScan returns the library's queued-or-running scan (or ErrNotFound
// when it is idle) — the guard behind the 409 on a second POST /scan.
func (d *DB) ActiveScan(libraryID int64) (Scan, error) {
	row := d.QueryRow(`SELECT `+scanColumns+` FROM scans WHERE library_id = ? AND state IN (?, ?) ORDER BY id DESC LIMIT 1`,
		libraryID, ScanQueued, ScanRunning)
	s, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Scan{}, ErrNotFound
	}
	return s, err
}

// StartScan marks a queued scan running (stamps started_at).
func (d *DB) StartScan(id int64) error {
	_, err := d.Exec(`UPDATE scans SET state = ?, started_at = datetime('now') WHERE id = ? AND state = ?`,
		ScanRunning, id, ScanQueued)
	return err
}

// FinishScan closes a scan out with its state and counters.
func (d *DB) FinishScan(id int64, state string, added, updated, skipped int, errMsg string) error {
	_, err := d.Exec(
		`UPDATE scans SET state = ?, finished_at = datetime('now'), added = ?, updated = ?, skipped = ?, error = COALESCE(NULLIF(?, ''), NULL) WHERE id = ?`,
		state, added, updated, skipped, errMsg, id)
	return err
}
