package tmdb_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/tmdb"
)

// fakeTMDB stands in for api.tmdb.org + image.tmdb.org. Every /3/ request
// is counted, which is what lets the cache-first behaviour be asserted
// (a second fetch of the same thing must add zero requests).
type fakeTMDB struct {
	api *httptest.Server
	img *httptest.Server

	mu    sync.Mutex
	calls map[string]int
}

func newFakeTMDB(t *testing.T) *fakeTMDB {
	t.Helper()
	f := &fakeTMDB{calls: map[string]int{}}
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls[r.URL.Path]++
		f.mu.Unlock()
		switch {
		case r.URL.Path == "/3/search/movie":
			writeJSON(w, searchMovies(r.URL.Query().Get("query")))
		case r.URL.Path == "/3/search/tv":
			writeJSON(w, searchTV(r.URL.Query().Get("query")))
		case strings.HasPrefix(r.URL.Path, "/3/movie/"):
			writeJSON(w, movieByID(r.URL.Path))
		case strings.HasPrefix(r.URL.Path, "/3/tv/"):
			if strings.Contains(r.URL.Path, "/season/") {
				writeJSON(w, seasonByPath(r.URL.Path))
			} else {
				writeJSON(w, tvByID())
			}
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	f.img = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.calls["image"]++
		f.mu.Unlock()
		_, _ = w.Write([]byte("fake-image-bytes"))
	}))
	t.Cleanup(f.api.Close)
	t.Cleanup(f.img.Close)
	return f
}

// count returns the number of /3/ (API) requests — image downloads
// excluded: they are not subject to the search rate limit.
func (f *fakeTMDB) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls["/3/search/movie"] + f.calls["/3/search/tv"] +
		f.calls["/3/movie/123"] + f.calls["/3/movie/124"] + f.calls["/3/movie/125"] +
		f.calls["/3/tv/456"] + f.calls["/3/tv/456/season/1"] + f.calls["/3/tv/456/season/2"]
}

func (f *fakeTMDB) client(t *testing.T) *tmdb.Client {
	t.Helper()
	c, err := tmdb.NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cl := tmdb.New("test-key", c)
	cl.APIBase = f.api.URL
	cl.ImageBase = f.img.URL
	cl.Log = func(string, ...any) {}
	return cl
}

// --- fixtures (the test library's titles) ---

func searchMovies(q string) string {
	switch q {
	case "Big Buck Bunny":
		// A decoy that must lose the match: same title, wrong year.
		return `{"results":[
			{"id":123,"title":"Big Buck Bunny","release_date":"2008-04-30"},
			{"id":999,"title":"Big Buck Bunny","release_date":"2009-01-01"}]}`
	case "Big Buck Bun":
		// No exact title match: pickHit must fall back to the first
		// (relevance-ordered) result that scores by year.
		return `{"results":[
			{"id":123,"title":"Big Buck Bunny","release_date":"2008-04-30"},
			{"id":999,"title":"Big Buck Bunny (Unrated)","release_date":"2009-01-01"}]}`
	case "Sintel":
		return `{"results":[{"id":124,"title":"Sintel","release_date":"2010-04-30"}]}`
	case "War for the Planet of the Apes":
		return `{"results":[{"id":125,"title":"War for the Planet of the Apes","release_date":"2017-07-14"}]}`
	}
	return `{"results":[]}`
}

func movieByID(path string) string {
	switch strings.TrimPrefix(path, "/3/movie/") {
	case "123":
		return `{"id":123,"release_date":"2008-04-30","overview":"A giant rabbit takes on three rodents.","vote_average":8.1,"poster_path":"/p123.jpg","backdrop_path":"/b123.jpg"}`
	case "124":
		return `{"id":124,"release_date":"2010-04-30","overview":"A girl searches for her dragon.","vote_average":7.9,"poster_path":"/p124.jpg","backdrop_path":"/b124.jpg"}`
	case "125":
		return `{"id":125,"release_date":"2017-07-14","overview":"Caesar's apes face the humans.","vote_average":7.1,"poster_path":"/p125.webp","backdrop_path":"/b125.webp"}`
	}
	return `{"id":0}`
}

func searchTV(q string) string {
	if q == "The Test Show" {
		return `{"results":[{"id":456,"name":"The Test Show","first_air_date":"2020-01-05"}]}`
	}
	return `{"results":[]}`
}

func tvByID() string {
	return `{"id":456,"first_air_date":"2020-01-05","overview":"A show for testing.","vote_average":6.4,"poster_path":"/p456.jpg","backdrop_path":"/b456.jpg"}`
}

func seasonByPath(path string) string {
	switch {
	case strings.HasSuffix(path, "/season/1"):
		return `{"air_date":"2020-01-05","episodes":[
			{"id":7001,"episode_number":1,"name":"Pilot","overview":"It begins.","vote_average":7,"air_date":"2020-01-05"},
			{"id":7002,"episode_number":2,"name":"Second","overview":"","vote_average":6.5,"air_date":"2020-01-12"},
			{"id":7003,"episode_number":3,"name":"Third","overview":"","vote_average":6.2,"air_date":"2020-01-19"}]}`
	case strings.HasSuffix(path, "/season/2"):
		return `{"air_date":"2020-07-01","episodes":[
			{"id":8001,"episode_number":1,"name":"The Return","overview":"","vote_average":6.9,"air_date":"2020-07-01"}]}`
	}
	return `{"air_date":"","episodes":[]}`
}

func writeJSON(w http.ResponseWriter, v string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(v))
}

// --- tests ---

func TestFetchMovie(t *testing.T) {
	f := newFakeTMDB(t)
	cl := f.client(t)

	m, err := cl.FetchMovie(context.Background(), "Big Buck Bunny", 2008)
	if err != nil {
		t.Fatal(err)
	}
	if m.TMDBID != 123 {
		t.Fatalf("TMDBID = %d, want 123 (the decoy lost the match)", m.TMDBID)
	}
	if m.Kind != "movie" || m.Rating != 8.1 || m.AirDate != "2008-04-30" || m.Overview == "" {
		t.Fatalf("unexpected meta: %+v", m)
	}
	if m.PosterKey != "123-poster.jpg" || m.BackdropKey != "123-backdrop.jpg" {
		t.Fatalf("artwork keys: %q / %q", m.PosterKey, m.BackdropKey)
	}
	for _, k := range []string{m.PosterKey, m.BackdropKey} {
		p := cl.Cache.Artwork(k)
		if p == "" {
			t.Fatalf("artwork file %s not on disk", k)
		}
		if b, err := os.ReadFile(p); err != nil || len(b) == 0 {
			t.Fatalf("artwork file %s empty: %v", k, err)
		}
	}
	// search + movie detail = 2 API calls.
	if n := f.count(); n != 2 {
		t.Fatalf("API calls = %d, want 2", n)
	}
}

// Fuzzy fallback: no search result has the exact scanned title, so the
// first (relevance-ordered) hit that scores by year wins.
func TestFetchMovieFuzzyFallback(t *testing.T) {
	f := newFakeTMDB(t)
	cl := f.client(t)
	m, err := cl.FetchMovie(context.Background(), "Big Buck Bun", 2008)
	if err != nil {
		t.Fatal(err)
	}
	if m.TMDBID != 123 {
		t.Fatalf("TMDBID = %d, want 123 (relevance fallback)", m.TMDBID)
	}
}

// Cache-first: a second fetch of the same movie makes zero network calls.
func TestFetchMovieCacheFirst(t *testing.T) {
	f := newFakeTMDB(t)
	cl := f.client(t)
	ctx := context.Background()

	m1, err := cl.FetchMovie(ctx, "Sintel", 2010)
	if err != nil {
		t.Fatal(err)
	}
	after := f.count()

	m2, err := cl.FetchMovie(ctx, "Sintel", 2010)
	if err != nil {
		t.Fatal(err)
	}
	if m1 != m2 {
		t.Fatalf("cached meta differs: %+v vs %+v", m1, m2)
	}
	if n := f.count(); n != after {
		t.Fatalf("second fetch made %d new API calls, want 0", n-after)
	}
}

func TestFetchSeriesAndSeason(t *testing.T) {
	f := newFakeTMDB(t)
	cl := f.client(t)
	ctx := context.Background()

	s, err := cl.FetchSeries(ctx, "The Test Show")
	if err != nil {
		t.Fatal(err)
	}
	if s.TMDBID != 456 || s.Kind != "series" || s.Rating != 6.4 {
		t.Fatalf("unexpected series meta: %+v", s)
	}

	sm, eps, err := cl.FetchSeason(ctx, "The Test Show", 1)
	if err != nil {
		t.Fatal(err)
	}
	if sm.Kind != "season" || sm.TMDBID != 456 || sm.AirDate != "2020-01-05" {
		t.Fatalf("unexpected season meta: %+v", sm)
	}
	if len(eps) != 3 {
		t.Fatalf("got %d episodes, want 3", len(eps))
	}
	ep1 := eps[1]
	if ep1.TMDBID != 7001 || ep1.Kind != "episode" || ep1.AirDate != "2020-01-05" {
		t.Fatalf("unexpected episode 1: %+v", ep1)
	}
	if ep1.PosterKey != s.PosterKey {
		t.Fatalf("episode poster %q != series poster %q", ep1.PosterKey, s.PosterKey)
	}

	// The second season call is a disk-cache hit: no new API calls.
	after := f.count()
	if _, _, err := cl.FetchSeason(ctx, "The Test Show", 1); err != nil {
		t.Fatal(err)
	}
	if n := f.count(); n != after {
		t.Fatalf("second season fetch made %d new API calls, want 0", n-after)
	}
}

// A 429 is waited out (Retry-After honoured) and the request retried.
func TestRateLimitBackoff(t *testing.T) {
	var mu sync.Mutex
	searched := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		limited := !searched && r.URL.Path == "/3/search/movie"
		if limited {
			searched = true
		}
		mu.Unlock()
		if limited {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		switch r.URL.Path {
		case "/3/search/movie":
			writeJSON(w, searchMovies("Sintel"))
		case "/3/movie/124":
			writeJSON(w, movieByID("/3/movie/124"))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(api.Close)
	t.Cleanup(img.Close)

	c, _ := tmdb.NewCache(t.TempDir())
	cl := tmdb.New("k", c)
	cl.APIBase = api.URL
	cl.ImageBase = img.URL
	cl.Log = func(string, ...any) {}

	start := time.Now()
	m, err := cl.FetchMovie(context.Background(), "Sintel", 2010)
	if err != nil {
		t.Fatalf("fetch after 429: %v", err)
	}
	if m.TMDBID != 124 {
		t.Fatalf("TMDBID = %d, want 124", m.TMDBID)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("returned in %v — the 429 wait was not honoured", elapsed)
	}
}

func TestNoKey(t *testing.T) {
	c, _ := tmdb.NewCache(t.TempDir())
	cl := tmdb.New("", c)
	if _, err := cl.FetchMovie(context.Background(), "Sintel", 2010); err == nil {
		t.Fatal("FetchMovie without a key should fail")
	}
}

// A corrupt cache entry is refetched, not served.
func TestCacheCorruptEntryIsRefetched(t *testing.T) {
	f := newFakeTMDB(t)
	cl := f.client(t)
	if err := cl.Cache.Put(tmdb.Key("movie", "Sintel", "2010"), []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.FetchMovie(context.Background(), "Sintel", 2010); err != nil {
		t.Fatal(err) // must refetch, not serve the corrupt entry
	}
	if b, ok := cl.Cache.Get(tmdb.Key("movie", "Sintel", "2010")); !ok || len(b) == 0 {
		t.Fatal("cache entry not rewritten after refetch")
	}
}
