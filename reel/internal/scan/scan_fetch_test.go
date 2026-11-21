package scan

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/db"
)

// countingFetcher is a Fetcher with canned metadata. Every call that
// misses its internal "disk cache" (the results map) is counted, which is
// what makes both properties testable: a first scan counts its fetches, a
// rescan of an unchanged library counts zero (RunWith must not even call
// the fetcher for skipped items).
type countingFetcher struct {
	mu      sync.Mutex
	calls   int
	results map[string]fetchResult
}

type fetchResult struct {
	meta db.ItemMeta
	eps  map[int]db.ItemMeta // seasons only
}

func newCountingFetcher() *countingFetcher {
	return &countingFetcher{results: map[string]fetchResult{}}
}

func (f *countingFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *countingFetcher) lookup(key string, miss func() fetchResult) (fetchResult, error) {
	f.mu.Lock()
	if r, ok := f.results[key]; ok {
		f.mu.Unlock()
		return r, nil // disk-cache hit: no count
	}
	f.calls++
	f.mu.Unlock()
	r := miss()
	f.mu.Lock()
	f.results[key] = r
	f.mu.Unlock()
	return r, nil
}

var (
	movieMeta  = db.ItemMeta{TMDBID: 1000, Kind: "movie", Overview: "o", Rating: 8,
		AirDate: "2008-04-30", PosterKey: "1000-poster.jpg", BackdropKey: "1000-backdrop.jpg"}
	seriesMeta = db.ItemMeta{TMDBID: 2000, Kind: "series", Overview: "o", Rating: 6,
		AirDate: "2020-01-05", PosterKey: "2000-poster.jpg", BackdropKey: "2000-backdrop.jpg"}
)

func epMeta(n int, airDate string) db.ItemMeta {
	return db.ItemMeta{TMDBID: int64(3000 + n), Kind: "episode", Overview: "e", Rating: 7,
		AirDate: airDate, PosterKey: seriesMeta.PosterKey, BackdropKey: seriesMeta.BackdropKey}
}

func (f *countingFetcher) FetchMovie(_ context.Context, title string, year int) (db.ItemMeta, error) {
	key := fmt.Sprintf("movie|%s|%d", title, year)
	r, err := f.lookup(key, func() fetchResult { return fetchResult{meta: movieMeta} })
	if err != nil {
		return db.ItemMeta{}, err
	}
	return r.meta, nil
}

func (f *countingFetcher) FetchSeries(_ context.Context, name string) (db.ItemMeta, error) {
	r, err := f.lookup("series|"+name, func() fetchResult { return fetchResult{meta: seriesMeta} })
	if err != nil {
		return db.ItemMeta{}, err
	}
	return r.meta, nil
}

func (f *countingFetcher) FetchSeason(_ context.Context, seriesName string, season int) (db.ItemMeta, map[int]db.ItemMeta, error) {
	key := fmt.Sprintf("season|%s|%d", seriesName, season)
	r, err := f.lookup(key, func() fetchResult {
		sm := seriesMeta
		sm.Kind = "season"
		switch season {
		case 1:
			return fetchResult{meta: sm, eps: map[int]db.ItemMeta{
				1: epMeta(1, "2020-01-05"), 2: epMeta(2, "2020-01-12"), 3: epMeta(3, "2020-01-19"),
			}}
		default:
			return fetchResult{meta: sm, eps: map[int]db.ItemMeta{1: epMeta(1, "2020-07-01")}}
		}
	})
	if err != nil {
		return db.ItemMeta{}, nil, err
	}
	return r.meta, r.eps, nil
}

func newTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "reel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// The acceptance property: a scan of an unchanged library makes zero
// fetcher calls the second time around.
func TestRunWithFetcherSecondRunNoCalls(t *testing.T) {
	d := newTestDB(t)
	f := newCountingFetcher()
	ctx := context.Background()

	libMovies, _ := d.CreateLibrary(db.Library{Name: "M", Path: moviesRoot, Type: "movies"})
	libShows, _ := d.CreateLibrary(db.Library{Name: "S", Path: showsRoot, Type: "shows"})

	r1, err := RunWith(ctx, d, libMovies, f)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Added != 3 {
		t.Fatalf("movies run 1: %+v (want 3 added)", r1)
	}
	r1s, err := RunWith(ctx, d, libShows, f)
	if err != nil {
		t.Fatal(err)
	}
	if r1s.Added != 6 { // 4 episodes + 2 season folders
		t.Fatalf("shows run 1: %+v (want 6 added)", r1s)
	}

	// Every item now carries metadata.
	for _, lib := range []db.Library{libMovies, libShows} {
		items, _ := d.ListItems(db.ItemFilter{LibraryID: &lib.ID})
		for _, i := range items {
			m, err := d.GetItemMeta(i.ID)
			if err != nil {
				t.Fatalf("item %q (%s) has no metadata: %v", i.Title, i.Type, err)
			}
			if m.TMDBID == 0 || m.Overview == "" {
				t.Fatalf("item %q: incomplete meta %+v", i.Title, m)
			}
			if i.Type == "episode" && m.PosterKey != "2000-poster.jpg" {
				t.Fatalf("episode %q: poster %q (want the series' poster)", i.Title, m.PosterKey)
			}
		}
	}

	after := f.count()

	// Run 2: everything unchanged → all skipped → zero fetcher calls.
	r2, err := RunWith(ctx, d, libMovies, f)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Skipped != 3 {
		t.Fatalf("movies run 2: %+v (want 3 skipped)", r2)
	}
	r2s, err := RunWith(ctx, d, libShows, f)
	if err != nil {
		t.Fatal(err)
	}
	if r2s.Skipped != 6 {
		t.Fatalf("shows run 2: %+v (want 6 skipped)", r2s)
	}
	if n := f.count(); n != after {
		t.Fatalf("second runs made %d fetcher calls, want 0", n-after)
	}
}

// A changed item (size) is updated and refetched; an unchanged sibling is
// not touched.
func TestRunWithFetcherUpdatedItemRefetches(t *testing.T) {
	d := newTestDB(t)
	f := newCountingFetcher()
	ctx := context.Background()
	lib, _ := d.CreateLibrary(db.Library{Name: "M", Path: moviesRoot, Type: "movies"})

	if _, err := RunWith(ctx, d, lib, f); err != nil {
		t.Fatal(err)
	}
	// A fresh fetcher (empty cache) so the refetch of the changed item is
	// counted.
	f2 := newCountingFetcher()

	// Simulate a changed file: touch one movie's size via a direct update.
	items, _ := d.ListItems(db.ItemFilter{LibraryID: &lib.ID, Type: "movie"})
	if len(items) == 0 {
		t.Fatal("no movie items")
	}
	victim := items[0]
	if err := d.UpdateItemMeta(victim.ID, db.Item{
		LibraryID: victim.LibraryID, Title: victim.Title, Path: victim.Path,
		Year: victim.Year, Size: victim.Size + 1, ModTime: victim.ModTime,
	}); err != nil {
		t.Fatal(err)
	}

	r, err := RunWith(ctx, d, lib, f2)
	if err != nil {
		t.Fatal(err)
	}
	// The walk sees the on-disk size (unchanged) against the bumped db size
	// → that item is "updated" and refetched; the other two are skipped.
	if r.Updated != 1 || r.Skipped != 2 {
		t.Fatalf("run after size change: %+v (want 1 updated, 2 skipped)", r)
	}
	if r2, _ := RunWith(ctx, d, lib, f2); r2.Updated != 0 {
		t.Fatalf("walk self-corrects on the next run: %+v", r2)
	}
	m, err := d.GetItemMeta(victim.ID)
	if err != nil {
		t.Fatalf("refetched item lost its metadata: %v", err)
	}
	if m.TMDBID != 1000 {
		t.Fatalf("refetched meta: %+v", m)
	}
}
