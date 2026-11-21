// The runner turns Walk's candidates into store rows. It lives next to
// the walker but is the only part of the package that touches the db:
// the parsing (Walk, parseFile) stays pure and unit-testable.
package scan

import (
	"context"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/reel/internal/db"
)

// Result is the outcome of one scan run, recorded on the scan row.
type Result struct {
	Added   int
	Updated int
	Skipped int
}

// Run walks the library tree and upserts every candidate into the store,
// without metadata fetching (the queue handler's no-key path).
func Run(d *db.DB, lib db.Library) (Result, error) {
	return RunWith(context.Background(), d, lib, nil)
}

// RunWith is Run plus optional TMDB metadata: every candidate the walk
// adds or changes is fetched through f and stored on its item row
// (best-effort — a fetch failure leaves the item without metadata but
// never fails the scan). Items the walk skips (unchanged) keep their
// metadata untouched — that is what makes a rescan of an unchanged
// library a no-op for the network. Candidates the walk no longer sees
// are kept as they are: the library root may be a partial mount and
// nothing in Reel deletes media metadata.
func RunWith(ctx context.Context, d *db.DB, lib db.Library, f Fetcher) (Result, error) {
	cands, err := Walk(lib.Path, lib.Type)
	if err != nil {
		return Result{}, err
	}
	var r Result
	for _, c := range cands {
		item := db.Item{
			LibraryID: lib.ID,
			Type:      c.Kind,
			Title:     c.Title,
			Path:      c.RelPath,
			Year:      c.Year,
			Season:    c.Season,
			Episode:   c.Episode,
			Size:      c.Size,
		}
		if !c.ModTime.IsZero() {
			item.ModTime = c.ModTime.Format(time.RFC3339)
		}
		// Episodes parent to their season item. Season candidates sort
		// before the files inside them, so the season row already exists.
		if c.Kind == "episode" {
			if season, err := d.FindItemByPath(lib.ID, seasonPathOf(c.RelPath)); err == nil {
				item.ParentID = &season.ID
			}
		}
		a, id, err := upsertItem(d, item)
		if err != nil {
			return r, err
		}
		switch a {
		case "added":
			r.Added++
		case "updated":
			r.Updated++
		default:
			r.Skipped++
		}
		if f != nil && a != "skipped" {
			// A renamed item's old metadata is stale: drop it so the
			// refetch starts clean (a failed refetch leaves none).
			if a == "updated" {
				_ = d.DeleteItemMeta(id)
			}
			fetchItemMeta(ctx, d, f, c, id)
		}
	}
	return r, nil
}

// seasonPathOf returns the "Show/Season NN" prefix of an episode rel path.
func seasonPathOf(rel string) string {
	parts := strings.Split(rel, "/")
	if len(parts) < 2 {
		return rel
	}
	return parts[0] + "/" + parts[1]
}

// upsertItem inserts or refreshes one candidate and reports what happened
// plus the item's id.
func upsertItem(d *db.DB, item db.Item) (string, int64, error) {
	existing, err := d.FindItemByPath(item.LibraryID, item.Path)
	if err != nil && err != db.ErrNotFound {
		return "", 0, err
	}
	if err == nil {
		changed := existing.Title != item.Title ||
			existing.Size != item.Size ||
			existing.ModTime != item.ModTime ||
			existing.Year != item.Year ||
			existing.Season != item.Season ||
			existing.Episode != item.Episode
		if !changed {
			return "skipped", existing.ID, nil
		}
		if err := d.UpdateItemMeta(existing.ID, item); err != nil {
			return "", 0, err
		}
		return "updated", existing.ID, nil
	}
	i, err := d.InsertItem(item)
	if err != nil {
		return "", 0, err
	}
	return "added", i.ID, nil
}
