package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/db"
)

const (
	// APIBase is the production TMDB v3 API.
	APIBase = "https://api.tmdb.org"
	// ImageBase is the production TMDB image host.
	ImageBase = "https://image.tmdb.org"
)

// Client resolves scanned media against TMDB. Lookups are cache-first: a
// key present in the disk cache is returned without any network call.
// The free tier is rate-limited (~10 req / 10 s / IP); a 429 is waited out
// via the X-RateLimit-Reset / Retry-After headers and retried.
type Client struct {
	APIBase   string
	ImageBase string
	Key       string
	Cache     *Cache
	HTTP      *http.Client
	Log       func(format string, args ...any)
}

// New wires a client against the production bases with a 15 s timeout.
func New(key string, c *Cache) *Client {
	return &Client{
		APIBase:   APIBase,
		ImageBase: ImageBase,
		Key:       key,
		Cache:     c,
		HTTP:      &http.Client{Timeout: 15 * time.Second},
		Log:       log.Printf,
	}
}

// --- response shapes (only the fields Reel uses) ---

type searchHit struct {
	ID           int64   `json:"id"`
	Title        string  `json:"title"`
	Name         string  `json:"name"`
	ReleaseDate  string  `json:"release_date"`
	FirstAirDate string  `json:"first_air_date"`
}

type searchPage struct {
	Results []searchHit `json:"results"`
}

type movieFull struct {
	ID           int64   `json:"id"`
	ReleaseDate  string  `json:"release_date"`
	Overview     string  `json:"overview"`
	VoteAverage  float64 `json:"vote_average"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
}

type seriesFull struct {
	ID           int64   `json:"id"`
	FirstAirDate string  `json:"first_air_date"`
	Overview     string  `json:"overview"`
	VoteAverage  float64 `json:"vote_average"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
}

type episodeFull struct {
	ID            int64   `json:"id"`
	EpisodeNumber int     `json:"episode_number"`
	Name          string  `json:"name"`
	Overview      string  `json:"overview"`
	VoteAverage   float64 `json:"vote_average"`
	AirDate       string  `json:"air_date"`
}

type seasonFull struct {
	AirDate  string        `json:"air_date"`
	Episodes []episodeFull `json:"episodes"`
}

// metaBlob is what the disk cache stores for one movie or series. The
// artwork file names live outside the db.ItemMeta: they are json:"-"
// there (internal cache names, not part of the API's metadata object),
// so the blob carries them separately.
type metaBlob struct {
	Meta        db.ItemMeta `json:"meta"`
	PosterKey   string      `json:"poster_key"`
	BackdropKey string      `json:"backdrop_key"`
}

func newMetaBlob(m db.ItemMeta) metaBlob {
	return metaBlob{Meta: m, PosterKey: m.PosterKey, BackdropKey: m.BackdropKey}
}

func (b metaBlob) meta() db.ItemMeta {
	m := b.Meta
	m.PosterKey = b.PosterKey
	m.BackdropKey = b.BackdropKey
	return m
}

// seasonCache is what the disk cache stores for one season: the season
// item's meta plus its episodes keyed by episode number.
type seasonCache struct {
	Meta metaBlob         `json:"meta"`
	Eps  map[int]metaBlob `json:"eps"`
}

// --- fetching ---

// FetchMovie resolves a movie by the scanned title and year. The year is
// part of the cache key and a primary search filter; matching prefers an
// exact (case-folded) title plus the year, and falls back to TMDB's own
// relevance ranking.
func (c *Client) FetchMovie(ctx context.Context, title string, year int) (db.ItemMeta, error) {
	if c.Key == "" {
		return db.ItemMeta{}, fmt.Errorf("tmdb: no API key")
	}
	key := Key("movie", title, strconv.Itoa(year))
	if b, ok := c.Cache.Get(key); ok {
		var mb metaBlob
		if json.Unmarshal(b, &mb) == nil {
			return mb.meta(), nil
		}
	}
	q := url.Values{"query": {title}}
	if year > 0 {
		q.Set("year", strconv.Itoa(year))
	}
	var page searchPage
	if err := c.getJSON(ctx, c.apiURL("/3/search/movie", q), &page); err != nil {
		return db.ItemMeta{}, err
	}
	id, ok := pickHit(page.Results, func(h searchHit) string { return h.Title },
		title, func(h searchHit) string { return h.ReleaseDate }, year)
	if !ok {
		return db.ItemMeta{}, fmt.Errorf("tmdb: no movie match for %q (%d)", title, year)
	}
	var f movieFull
	if err := c.getJSON(ctx, c.apiURL(fmt.Sprintf("/3/movie/%d?language=en-US", id), nil), &f); err != nil {
		return db.ItemMeta{}, err
	}
	m := db.ItemMeta{
		TMDBID:   f.ID,
		Kind:     "movie",
		Overview: f.Overview,
		Rating:   f.VoteAverage,
		AirDate:  f.ReleaseDate,
	}
	c.images(&m, f.PosterPath, f.BackdropPath)
	_ = c.Cache.Put(key, mustJSON(newMetaBlob(m)))
	return m, nil
}

// FetchSeries resolves a show by its scanned name.
func (c *Client) FetchSeries(ctx context.Context, name string) (db.ItemMeta, error) {
	if c.Key == "" {
		return db.ItemMeta{}, fmt.Errorf("tmdb: no API key")
	}
	key := Key("series", name)
	if b, ok := c.Cache.Get(key); ok {
		var mb metaBlob
		if json.Unmarshal(b, &mb) == nil {
			return mb.meta(), nil
		}
	}
	var page searchPage
	if err := c.getJSON(ctx, c.apiURL("/3/search/tv", url.Values{"query": {name}}), &page); err != nil {
		return db.ItemMeta{}, err
	}
	id, ok := pickHit(page.Results, func(h searchHit) string { return h.Name },
		name, nil, 0)
	if !ok {
		return db.ItemMeta{}, fmt.Errorf("tmdb: no tv match for %q", name)
	}
	var f seriesFull
	if err := c.getJSON(ctx, c.apiURL(fmt.Sprintf("/3/tv/%d?language=en-US", id), nil), &f); err != nil {
		return db.ItemMeta{}, err
	}
	m := db.ItemMeta{
		TMDBID:   f.ID,
		Kind:     "series",
		Overview: f.Overview,
		Rating:   f.VoteAverage,
		AirDate:  f.FirstAirDate,
	}
	c.images(&m, f.PosterPath, f.BackdropPath)
	_ = c.Cache.Put(key, mustJSON(newMetaBlob(m)))
	return m, nil
}

// FetchSeason resolves a show's season: the first return is the season
// item's metadata (the series' id and artwork, the season's air date, kind
// "season"), the second the episode metadata keyed by episode number
// (episode artwork is the series' artwork — TMDB still images are sparse
// and the series image is what a TV UI shows for an episode row).
func (c *Client) FetchSeason(ctx context.Context, seriesName string, season int) (db.ItemMeta, map[int]db.ItemMeta, error) {
	key := Key("season", seriesName, strconv.Itoa(season))
	if b, ok := c.Cache.Get(key); ok {
		var sc seasonCache
		if json.Unmarshal(b, &sc) == nil {
			eps := make(map[int]db.ItemMeta, len(sc.Eps))
			for n, e := range sc.Eps {
				eps[n] = e.meta()
			}
			return sc.Meta.meta(), eps, nil
		}
	}
	series, err := c.FetchSeries(ctx, seriesName) // itself cache-first
	if err != nil {
		return db.ItemMeta{}, nil, err
	}
	var sf seasonFull
	if err := c.getJSON(ctx, c.apiURL(fmt.Sprintf("/3/tv/%d/season/%d", series.TMDBID, season), nil), &sf); err != nil {
		return db.ItemMeta{}, nil, err
	}
	m := series
	m.Kind = "season"
	m.AirDate = sf.AirDate
	eps := make(map[int]db.ItemMeta, len(sf.Episodes))
	epBlobs := make(map[int]metaBlob, len(sf.Episodes))
	for _, e := range sf.Episodes {
		em := db.ItemMeta{
			TMDBID:      e.ID,
			Kind:        "episode",
			Overview:    e.Overview,
			Rating:      e.VoteAverage,
			AirDate:     e.AirDate,
			PosterKey:   series.PosterKey,
			BackdropKey: series.BackdropKey,
		}
		eps[e.EpisodeNumber] = em
		epBlobs[e.EpisodeNumber] = newMetaBlob(em)
	}
	_ = c.Cache.Put(key, mustJSON(seasonCache{Meta: newMetaBlob(m), Eps: epBlobs}))
	return m, eps, nil
}

// images downloads the poster (w500) and backdrop (w1280) into the
// artwork cache. Failures are logged and leave the key empty — metadata
// is still useful without a missing image.
func (c *Client) images(m *db.ItemMeta, posterPath, backdropPath string) {
	if posterPath != "" {
		if b, err := c.image("w500", posterPath); err == nil {
			k := artworkKey(m.TMDBID, "poster", posterPath)
			if err := c.Cache.SaveArtwork(k, b); err != nil {
				c.Log("tmdb: save poster: %v", err)
			} else {
				m.PosterKey = k
			}
		} else {
			c.Log("tmdb: poster: %v", err)
		}
	}
	if backdropPath != "" {
		if b, err := c.image("w1280", backdropPath); err == nil {
			k := artworkKey(m.TMDBID, "backdrop", backdropPath)
			if err := c.Cache.SaveArtwork(k, b); err != nil {
				c.Log("tmdb: save backdrop: %v", err)
			} else {
				m.BackdropKey = k
			}
		} else {
			c.Log("tmdb: backdrop: %v", err)
		}
	}
}

func (c *Client) image(size, path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.ImageBase+"/t/p/"+size+path, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image %s: status %d", path, res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	return b, nil
}

// artworkKey names a cached image file: "<tmdb-id>-<kind><ext>".
func artworkKey(id int64, kind, path string) string {
	return fmt.Sprintf("%d-%s%s", id, kind, filepath.Ext(path))
}

// --- matching ---

// pickHit scores the search results against the scanned name: an exact
// (case-folded) title is worth 2, a matching year worth 1. The best
// scoring hit wins (ties keep TMDB's relevance order); when nothing
// scores, the first result is taken — TMDB already ranks by relevance,
// which is the fuzzy fallback the scan job asks for.
func pickHit(rs []searchHit, title func(h searchHit) string, want string,
	date func(h searchHit) string, year int) (int64, bool) {
	if len(rs) == 0 {
		return 0, false
	}
	best, bestScore := 0, -1
	for i, r := range rs {
		s := 0
		if strings.EqualFold(title(r), want) {
			s += 2
		}
		if year > 0 && date != nil && strings.HasPrefix(date(r), strconv.Itoa(year)) {
			s++
		}
		if s > bestScore {
			best, bestScore = i, s
		}
	}
	return rs[best].ID, true
}

// --- HTTP with rate-limit backoff ---

// getJSON issues GET rawURL into out, waiting out 429s (up to 3 waits).
func (c *Client) getJSON(ctx context.Context, rawURL string, out any) error {
	log := c.Log
	if log == nil {
		log = func(string, ...any) {}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		res, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		_ = res.Body.Close()
		if err != nil {
			return err
		}
		if res.StatusCode == http.StatusTooManyRequests {
			if attempt >= 3 {
				return fmt.Errorf("tmdb: rate limited (429) after %d waits", attempt)
			}
			wait := rateLimitWait(res.Header)
			log("tmdb: 429 — waiting %v", wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("tmdb: status %d: %s", res.StatusCode, truncate(string(body), 200))
		}
		return json.Unmarshal(body, out)
	}
}

// rateLimitWait reads the 429 headers (X-RateLimit-Reset as unix seconds,
// or Retry-After as a duration) into a wait of at least 1 s, capped at 60 s.
func rateLimitWait(h http.Header) time.Duration {
	var until time.Time
	if v := h.Get("X-RateLimit-Reset"); v != "" {
		if s, err := strconv.ParseInt(v, 10, 64); err == nil {
			until = time.Unix(s, 0)
		}
	}
	if until.IsZero() {
		if v := h.Get("Retry-After"); v != "" {
			if s, err := strconv.Atoi(v); err == nil {
				until = time.Now().Add(time.Duration(s) * time.Second)
			}
		}
	}
	if until.IsZero() {
		return time.Second
	}
	wait := time.Until(until) + time.Second
	if wait < time.Second {
		wait = time.Second
	}
	if wait > 60*time.Second {
		wait = 60 * time.Second
	}
	return wait
}

// apiURL prefixes the API base and appends the key and query.
func (c *Client) apiURL(path string, extra url.Values) string {
	v := url.Values{}
	for k, vs := range extra {
		v[k] = vs
	}
	v.Set("api_key", c.Key)
	return c.APIBase + path + "?" + v.Encode()
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
