package scan

import (
	"context"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/db"
)

// Fetcher resolves TMDB metadata for scanned items (the internal/tmdb
// client is the production implementation). RunWith calls it only for
// items the walk added or changed, so a rescan of an unchanged library
// makes zero network calls; implementations are expected to serve their
// own disk cache before touching the network at all.
type Fetcher interface {
	FetchMovie(ctx context.Context, title string, year int) (db.ItemMeta, error)
	FetchSeries(ctx context.Context, name string) (db.ItemMeta, error)
	FetchSeason(ctx context.Context, seriesName string, season int) (db.ItemMeta, map[int]db.ItemMeta, error)
}

// fetchItemMeta resolves one candidate's metadata and stores it on the
// item row. Every failure is dropped (the fetcher logs it): metadata is
// an enhancement, and one dead title must not fail a whole scan.
func fetchItemMeta(ctx context.Context, d *db.DB, f Fetcher, c Candidate, id int64) {
	switch c.Kind {
	case "movie":
		if m, err := f.FetchMovie(ctx, c.Title, c.Year); err == nil {
			m.Kind = "movie"
			_ = d.UpsertItemMeta(id, m)
		}
	case "season":
		show := showNameOf(c.RelPath)
		series, err := f.FetchSeries(ctx, show)
		if err != nil {
			return
		}
		sm, _, err := f.FetchSeason(ctx, show, c.Season)
		if err != nil {
			sm = series // season detail failed: the series' metadata still stands
		}
		sm.Kind = "season"
		_ = d.UpsertItemMeta(id, sm)
	case "episode":
		show := showNameOf(c.RelPath)
		series, err := f.FetchSeries(ctx, show)
		if err != nil {
			return
		}
		_, eps, err := f.FetchSeason(ctx, show, c.Season)
		if err != nil {
			return
		}
		ep, ok := eps[c.Episode]
		if !ok {
			return // the season exists but this episode number is not in it
		}
		// Episode artwork is the series' artwork (documented in the
		// README; TMDB still images are sparse for episodes).
		ep.Kind = "episode"
		ep.PosterKey, ep.BackdropKey = series.PosterKey, series.BackdropKey
		_ = d.UpsertItemMeta(id, ep)
	}
}

// showNameOf takes the first path segment of a rel path: the show folder.
func showNameOf(rel string) string {
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return rel
}
