package db

import (
	"errors"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "reel.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestLibraryRoundTrip(t *testing.T) {
	d := openTemp(t)
	l, err := d.CreateLibrary(Library{Name: "Movies", Path: "/data/movies", Type: "movies"})
	if err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}
	if l.ID == 0 {
		t.Fatal("expected non-zero id")
	}
	got, err := d.GetLibrary(l.ID)
	if err != nil {
		t.Fatalf("GetLibrary: %v", err)
	}
	if got != l {
		t.Fatalf("got %+v, want %+v", got, l)
	}
	if _, err := d.GetLibrary(999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetLibrary(999): %v, want ErrNotFound", err)
	}
	all, err := d.ListLibraries()
	if err != nil {
		t.Fatalf("ListLibraries: %v", err)
	}
	if len(all) != 1 || all[0].Name != "Movies" {
		t.Fatalf("ListLibraries: %+v", all)
	}
}

func TestItemFilters(t *testing.T) {
	d := openTemp(t)
	lib, _ := d.CreateLibrary(Library{Name: "Shows", Path: "/data/shows", Type: "shows"})
	season, _ := d.InsertItem(Item{LibraryID: lib.ID, Type: "season", Title: "S1", Path: "/s1"})
	movie, _ := d.InsertItem(Item{LibraryID: lib.ID, Type: "movie", Title: "Big Buck Bunny", Path: "/bbb"})
	ep, _ := d.InsertItem(Item{LibraryID: lib.ID, Type: "episode", Title: "E1", Path: "/e1", ParentID: &season.ID})

	one := lib.ID
	got, err := d.ListItems(ItemFilter{LibraryID: &one, Type: "episode"})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(got) != 1 || got[0].ID != ep.ID || got[0].ParentID == nil || *got[0].ParentID != season.ID {
		t.Fatalf("episode filter: %+v", got)
	}
	got, err = d.ListItems(ItemFilter{Query: "buck", Limit: 10})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(got) != 1 || got[0].ID != movie.ID {
		t.Fatalf("query filter: %+v", got)
	}
	if _, err := d.GetItem(42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetItem(42): %v, want ErrNotFound", err)
	}
}
