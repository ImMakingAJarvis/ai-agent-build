// Package db is the SQLite store. Schema v0: users, libraries, items
// (movies, seasons, episodes linked by parent_id).
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
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_items_library ON items(library_id);
CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent_id);
`

// DB wraps *sql.DB.
type DB struct{ *sql.DB }

// Library is one media library. JSON names are the spec's field names.
type Library struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
}

// Item is one media item (movie, season or episode).
type Item struct {
	ID        int64  `json:"id"`
	LibraryID int64  `json:"library_id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Path      string `json:"path"`
	ParentID  *int64 `json:"parent_id"`
}

// ItemFilter narrows ListItems.
type ItemFilter struct {
	LibraryID *int64
	Type      string
	Query     string
	Limit     int
}

// Open opens (creating if needed) the SQLite file and applies the schema.
func Open(path string) (*DB, error) {
	d, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(schema); err != nil {
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
	if err := rows.Scan(&i.ID, &i.LibraryID, &i.Type, &i.Title, &i.Path, &i.ParentID); err != nil {
		return i, err
	}
	return i, nil
}

// InsertItem inserts an item and returns it with its id.
func (d *DB) InsertItem(i Item) (Item, error) {
	res, err := d.Exec(
		`INSERT INTO items (library_id, type, title, path, parent_id) VALUES (?, ?, ?, ?, ?)`,
		i.LibraryID, i.Type, i.Title, i.Path, i.ParentID)
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
	q := `SELECT id, library_id, type, title, path, parent_id FROM items WHERE ` +
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
	row := d.QueryRow(`SELECT id, library_id, type, title, path, parent_id FROM items WHERE id = ?`, id)
	i, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("get item: %w", err)
	}
	return i, nil
}
