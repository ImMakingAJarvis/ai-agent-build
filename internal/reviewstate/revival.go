package reviewstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Revival is one line of the revival log: the five-minute check requeued a
// missing next job, which is a bug. At is stored in UTC.
type Revival struct {
	At       time.Time `json:"at"`
	PR       int       `json:"pr"`
	State    string    `json:"state"`
	Enqueued string    `json:"enqueued"`
}

// AppendRevival appends one JSON object per line to dir/revivals.jsonl,
// created with mode 0600.
func AppendRevival(dir string, rv Revival) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	rv.At = rv.At.UTC()
	b, err := json.Marshal(rv)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(filepath.Join(dir, "revivals.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return err
	}
	return nil
}

// losAngeles is the calendar the review day runs on.
var losAngeles = loadZone("America/Los_Angeles")

func loadZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		// A process that cannot tell Los Angeles days apart must not
		// guess; failing at startup is the honest outcome.
		panic(fmt.Sprintf("reviewstate: %v", err))
	}
	return loc
}

// RevivalsOn returns the revival lines whose At falls on the calendar day
// of day in America/Los_Angeles. A line from 07:30 UTC is the previous
// evening in Los Angeles during standard time (UTC-8). A missing log is
// an empty result, not an error.
func RevivalsOn(dir string, day time.Time) ([]Revival, error) {
	lo := day.In(losAngeles)
	start := time.Date(lo.Year(), lo.Month(), lo.Day(), 0, 0, 0, 0, losAngeles)
	end := start.AddDate(0, 0, 1)

	data, err := os.ReadFile(filepath.Join(dir, "revivals.jsonl"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Revival
	for len(bytes.TrimSpace(data)) > 0 {
		line, rest, _ := bytes.Cut(data, []byte("\n"))
		data = rest
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rv Revival
		if err := json.Unmarshal(line, &rv); err != nil {
			return nil, fmt.Errorf("reviewstate: revivals.jsonl: %w", err)
		}
		if !rv.At.Before(start) && rv.At.Before(end) {
			out = append(out, rv)
		}
	}
	return out, nil
}
