// Package scan walks a library's media tree and parses item metadata from
// the file layout and names. Two layouts are supported (the plan's scan
// job): movies as "Name (Year)/<file>" and TV as
// "Show/Season 01/<file>" with SxxEyy episode naming. The walker is pure
// (no DB, no network) so the parser is unit-testable against the test
// library in reel/testdata.
package scan

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MediaExts are the video container extensions the scanner picks up.
var MediaExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".mov": true,
	".m4v": true, ".webm": true, ".ts": true, ".m2ts": true,
	".wmv": true, ".flv": true, ".ogv": true,
}

// Candidate is one parsed media file (or season folder) ready to upsert.
type Candidate struct {
	RelPath string // path relative to the library root (stable id)
	Kind    string // "movie" | "season" | "episode"
	Title   string
	Year    int // movies
	Season  int // episodes (and season items)
	Episode int // episodes
	Size    int64
	ModTime time.Time
}

// IsMedia reports whether name is a media file the scanner picks up.
// Hidden files and dot-prefixed siblings are skipped.
func IsMedia(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	return MediaExts[strings.ToLower(filepath.Ext(name))]
}

var (
	// movieDir: "Name (Year)" — anything after the closing paren is
	// ignored ("Name (Year) [1080p]", "Name (2008) (dir cut)").
	movieDirRe = regexp.MustCompile(`^(?P<name>.+?)\s*\((?P<year>\d{4})\)(?:\s.*)?$`)
	// seasonDir: "Season 01" / "Season 1" (case-insensitive).
	seasonDirRe = regexp.MustCompile(`(?i)^Season\s*0*(\d+)$`)
	// episode file: S01E02 anywhere in the stem ("S01E02 - Title.mkv",
	// "show.s01e02.1080p.mkv", "S01E02.mkv").
	episodeRe = regexp.MustCompile(`(?i)\bS(\d{1,2})E(\d{1,3})\b`)
	// episode title: the text after the SxxEyy token, once separators
	// (".", "_", "-", " ") are stripped from the front.
	titleSepRe = regexp.MustCompile(`^[._\-\s]+`)
)

// Walk parses the media under root (a library path) for a library of the
// given type ("movies" or "shows") and returns the candidates in stable
// (sorted RelPath) order. Unparseable trees yield no candidates, not an
// error — a half-named folder is skipped, not fatal to a scan.
func Walk(root, libType string) ([]Candidate, error) {
	var out []Candidate
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d == nil {
				return err // the root itself is unreadable: real error
			}
			return nil // unreadable sub-entry: skip it
		}
		if d.IsDir() || !IsMedia(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		out = append(out, parseFile(rel, info.Size(), info.ModTime(), libType))
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Season folders are items even though they hold no media of their
	// own: emit one per "Show/Season NN" directory that has media below.
	if libType == "shows" {
		out = append(out, seasonDirs(root)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}

// qualityTokens are name suffixes that are NOT episode titles
// ("S01E02.1080p.mkv" has no title, not one called "1080p").
var qualityTokens = map[string]bool{
	"2160p": true, "1080p": true, "1080i": true, "720p": true,
	"480p": true, "480i": true, "576p": true,
	"x264": true, "x265": true, "h264": true, "h265": true,
	"remux": true, "blu-ray": true, "bluray": true, "webrip": true,
	"hdtv": true, "dvdrip": true,
}

// parseFile turns one media file into a movie or episode candidate
// (zero Candidate when the path does not fit the layout).
func parseFile(rel string, size int64, mod time.Time, libType string) Candidate {
	parts := strings.Split(rel, "/")
	name := parts[len(parts)-1]
	depth := len(parts) - 1
	if libType == "shows" && depth >= 2 {
		// The second path segment must be a season folder.
		if sm := seasonDirRe.FindStringSubmatch(parts[1]); sm != nil {
			if span := episodeRe.FindStringIndex(name); span != nil {
				gm := episodeRe.FindStringSubmatch(name) // gm[1]=S number, gm[2]=E number
				e, _ := strconv.Atoi(gm[2])
				season, _ := strconv.Atoi(sm[1])
				title := "Episode " + strconv.Itoa(e)
				// Text after "SxxEyy" is the title, when present:
				// "S01E02 - Pilot.mkv" → "Pilot".
				stem := strings.TrimSuffix(name, filepath.Ext(name))
				if t := cleanEpisodeTitle(stem[span[1]:]); t != "" {
					title = t
				}
				return Candidate{RelPath: rel, Kind: "episode", Title: title,
					Season: season, Episode: e, Size: size, ModTime: mod}
			}
		}
		return Candidate{}
	}
	if libType == "movies" && depth >= 1 {
		if m := movieDirRe.FindStringSubmatch(parts[0]); m != nil {
			year, _ := strconv.Atoi(m[2])
			if year < 1890 || year > 2099 {
				return Candidate{}
			}
			title := m[1]
			// A movie file that itself carries the year ("Name (Year).mkv")
			// is the folder's canonical file; the folder name wins.
			return Candidate{RelPath: rel, Kind: "movie", Title: title,
				Year: year, Size: size, ModTime: mod}
		}
	}
	return Candidate{}
}

// cleanEpisodeTitle turns the text after SxxEyy into a title: separator
// junk off the front, dots to spaces, and trailing quality/tag tokens
// ("1080p", "x265") dropped — "S01E02.1080p.mkv" yields no title.
func cleanEpisodeTitle(s string) string {
	s = strings.Trim(s, "._- ")
	s = titleSepRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, ".", " ")
	s = strings.TrimSpace(s)
	fields := strings.Fields(s)
	for len(fields) > 0 {
		t := strings.ToLower(fields[len(fields)-1])
		if !qualityTokens[t] {
			break
		}
		fields = fields[:len(fields)-1]
	}
	return strings.Join(fields, " ")
}

// seasonDirs emits one season candidate per "Show/Season NN" directory
// that contains at least one media file (recursively).
func seasonDirs(root string) []Candidate {
	var out []Candidate
	// Walk already read this tree successfully above: a failure here (a
	// permission change mid-scan) yields no season items, not an error.
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		m := seasonDirRe.FindStringSubmatch(base)
		if m == nil || path == root {
			return nil
		}
		season, _ := strconv.Atoi(m[1])
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		hasMedia := false
		// A failed walk just means no visible media in this directory.
		_ = filepath.WalkDir(path, func(p string, fd fs.DirEntry, e error) error {
			if e == nil && !fd.IsDir() && IsMedia(fd.Name()) {
				hasMedia = true
			}
			return nil
		})
		if !hasMedia {
			return nil
		}
		out = append(out, Candidate{
			RelPath: filepath.ToSlash(rel), Kind: "season",
			Title: "Season " + strconv.Itoa(season), Season: season,
		})
		return nil
	})
	return out
}
