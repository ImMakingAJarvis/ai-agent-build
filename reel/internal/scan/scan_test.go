package scan

import (
	"path/filepath"
	"testing"
	"time"
)

var testTime = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// The test library lives in reel/testdata (see reel/testdata/README.md):
// movies as "Name (Year)/<file>", shows as "Show/Season NN/<SxxEyy file>".
const (
	moviesRoot = "../../testdata/movies"
	showsRoot  = "../../testdata/shows"
)

func byRel(cands []Candidate) map[string]Candidate {
	out := make(map[string]Candidate, len(cands))
	for _, c := range cands {
		out[c.RelPath] = c
	}
	return out
}

func TestWalkMovies(t *testing.T) {
	cands, err := Walk(moviesRoot, "movies")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	m := byRel(cands)
	if len(m) != 3 {
		t.Fatalf("want 3 movies, got %d: %v", len(m), cands)
	}
	for _, want := range []struct {
		rel   string
		title string
		year  int
		kind  string
	}{
		{"Big Buck Bunny (2008)/Big Buck Bunny (2008).mkv", "Big Buck Bunny", 2008, "movie"},
		{"Sintel (2010)/sintel.mp4", "Sintel", 2010, "movie"},
		{"War for the Planet of the Apes (2017) [1080p]/war.2017.1080p.mp4", "War for the Planet of the Apes", 2017, "movie"},
	} {
		c, ok := m[want.rel]
		if !ok {
			t.Errorf("missing %q", want.rel)
			continue
		}
		if c.Kind != want.kind || c.Title != want.title || c.Year != want.year {
			t.Errorf("%q: got kind=%q title=%q year=%d, want %q %q %d", want.rel, c.Kind, c.Title, c.Year, want.kind, want.title, want.year)
		}
		if c.Size <= 0 {
			t.Errorf("%q: size not carried through", want.rel)
		}
	}
}

func TestWalkShows(t *testing.T) {
	cands, err := Walk(showsRoot, "shows")
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	m := byRel(cands)
	// 4 episodes + 2 season folders; ABOUT.txt and .hidden.mkv are not media.
	if len(m) != 6 {
		t.Fatalf("want 6 candidates, got %d: %v", len(m), cands)
	}
	for _, want := range []struct {
		rel     string
		kind    string
		title   string
		season  int
		episode int
	}{
		{"The Test Show/Season 01/The Test Show S01E01 - Pilot.mp4", "episode", "Pilot", 1, 1},
		{"The Test Show/Season 01/The Test Show S01E02.mp4", "episode", "Episode 2", 1, 2},
		{"The Test Show/Season 01/the.test.show.s01e03.720p.mp4", "episode", "Episode 3", 1, 3},
		{"The Test Show/Season 02/S02E01 - The Return.mp4", "episode", "The Return", 2, 1},
		{"The Test Show/Season 01", "season", "Season 1", 1, 0},
		{"The Test Show/Season 02", "season", "Season 2", 2, 0},
	} {
		c, ok := m[want.rel]
		if !ok {
			t.Errorf("missing %q", want.rel)
			continue
		}
		if c.Kind != want.kind || c.Title != want.title || c.Season != want.season || c.Episode != want.episode {
			t.Errorf("%q: got kind=%q title=%q s=%d e=%d, want %q %q s=%d e=%d",
				want.rel, c.Kind, c.Title, c.Season, c.Episode, want.kind, want.title, want.season, want.episode)
		}
	}
}

func TestWalkIsStableAndPure(t *testing.T) {
	a, err := Walk(showsRoot, "shows")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Walk(showsRoot, "shows")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("unstable walk: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].RelPath != b[i].RelPath || a[i].Title != b[i].Title {
			t.Fatalf("unstable order at %d: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestParseFileOddNames(t *testing.T) {
	cases := []struct {
		name string
		rel  string
		typ  string
		want Candidate
	}{
		// odd episode spellings
		{rel: "Show/Season 1/S1E1.mkv", typ: "shows",
			want: Candidate{Kind: "episode", Season: 1, Episode: 1, Title: "Episode 1"}},
		{rel: "Show/Season 1/show.s01e01.1080p.webrip.x265.mkv", typ: "shows",
			want: Candidate{Kind: "episode", Season: 1, Episode: 1, Title: "Episode 1"}},
		{rel: "Show/Season 1/S01E01.The First One.mkv", typ: "shows",
			want: Candidate{Kind: "episode", Season: 1, Episode: 1, Title: "The First One"}},
		// files outside the layout are skipped, not errors
		{rel: "Show/Extras/thing.mkv", typ: "shows"},
		{rel: "loose.mkv", typ: "shows"},
		{rel: "Show/Season 1/not-an-episode.mkv", typ: "shows"},
		// movies: folder name decides; odd suffixes ignored
		{rel: "A Quiet Place (2018) (dir cut)/a.mkv", typ: "movies",
			want: Candidate{Kind: "movie", Title: "A Quiet Place", Year: 2018}},
		{rel: "NoYearHere/file.mkv", typ: "movies"},
		{rel: "Bad Year (1799)/file.mkv", typ: "movies"}, // year out of range
		// a movies-library file at the root is not a movie
		{rel: "Big Buck Bunny (2008).mkv", typ: "movies"},
	}
	for _, tc := range cases {
		got := parseFile(tc.rel, 1, testTime, tc.typ)
		if tc.want == (Candidate{}) {
			if got != (Candidate{}) {
				t.Errorf("parseFile(%q, %s): want skip, got %+v", tc.rel, tc.typ, got)
			}
			continue
		}
		if got.Kind != tc.want.Kind || got.Title != tc.want.Title ||
			got.Year != tc.want.Year || got.Season != tc.want.Season || got.Episode != tc.want.Episode {
			t.Errorf("parseFile(%q, %s) = %+v, want %+v", tc.rel, tc.typ, got, tc.want)
		}
	}
}

func TestIsMedia(t *testing.T) {
	for _, want := range []struct {
		name string
		ok   bool
	}{
		{"a.mkv", true}, {"B.MP4", true}, {"c.MOV", true},
		{"d.MKV ", false}, // trailing space: not an extension
		{"readme.txt", false}, {"", false}, {".hidden.mkv", false},
		{"noext", false}, {"e.avif", false},
	} {
		if got := IsMedia(want.name); got != want.ok {
			t.Errorf("IsMedia(%q) = %v, want %v", want.name, got, want.ok)
		}
	}
}

func TestWalkEmptyDir(t *testing.T) {
	dir := t.TempDir()
	cands, err := Walk(dir, "movies")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("want no candidates, got %v", cands)
	}
}

func TestWalkMissingRoot(t *testing.T) {
	if _, err := Walk(filepath.Join(t.TempDir(), "nope"), "movies"); err == nil {
		t.Fatal("want error for missing root")
	}
}
