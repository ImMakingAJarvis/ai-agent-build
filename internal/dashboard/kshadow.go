package dashboard

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kq"
	ks "github.com/ImMakingAJarvis/ai-agent-build/internal/kshadow"
)

// The Kafka shadow section (plan §5): the dashboard's glance at the phase-1
// dual-write mirror. It renders the same parity report `jarvis-kq parity`
// prints — kshadow's state file (the kshadow container bind-mounts the
// directory over /data), the RabbitMQ management counters and an
// independent scan of the source=amqp Kafka records. The report is cached
// 60s server-side: the Kafka scan is the expensive part and the tile is a
// health dot, not a live counter.

// kshadowSectionData is what the kafka-shadow-section template renders.
type kshadowSectionData struct {
	Report      *ks.Report
	Err         string // set when the report could not be built (Kafka down)
	Status      string // ok | mismatch | lag | down
	StatusClass string
	UpFor       string // kshadow's age, relative (no absolute timestamps in the tile)
}

// minusInt renders the report's "-1 = unknown" sentinel as "-".
func minusInt(n int64) string {
	if n < 0 {
		return "-"
	}
	return strconv.FormatInt(n, 10)
}

// kshadowReport builds (or returns the fresh cache of) the parity report.
func (s *Server) kshadowReport() (*ks.Report, error) {
	s.kshadowMu.Lock()
	cached, at := s.kshadowRep, s.kshadowAt
	s.kshadowMu.Unlock()
	if cached != nil && time.Since(at) < 60*time.Second {
		return cached, nil
	}
	state, err := ks.LoadState(s.kshadowState)
	if err != nil {
		return nil, err
	}
	admin, err := ks.NewAdmin(s.amqpURL)
	if err != nil {
		return nil, err
	}
	rep, err := (&ks.Parity{State: state, Admin: admin, Kafka: kq.New(s.kafkaBrokers...)}).Report(context.Background())
	if err != nil {
		return nil, err
	}
	// The phase-2 dry-run section (its state sits next to the mirror's).
	if _, serr := os.Stat(filepath.Join(filepath.Dir(s.kshadowState), "kshadow-dryrun.json")); serr == nil {
		if dst, derr := ks.LoadDryRunState(filepath.Join(filepath.Dir(s.kshadowState), "kshadow-dryrun.json")); derr == nil {
			deadNew := dst.DeadNew
			if st, aerr := admin.QueueStats(context.Background(), "dead"); aerr == nil && dst.DeadBase > 0 {
				deadNew = st.Ready + st.Unacked + st.Delivered - dst.DeadBase
			}
			rep.DryRun = ks.DryRunReportFromState(dst, deadNew)
		}
	}
	s.kshadowMu.Lock()
	s.kshadowRep, s.kshadowAt = rep, time.Now()
	s.kshadowMu.Unlock()
	return rep, nil
}

func (s *Server) handleKafkaShadowHTML(w http.ResponseWriter, r *http.Request) {
	rep, err := s.kshadowReport()
	data := kshadowSectionData{Status: "down", StatusClass: "bad"}
	if err != nil {
		data.Err = err.Error()
	} else {
		data.Report = rep
		data.Status, data.StatusClass = kshadowStatus(rep)
		if rep.StartedAt != (time.Time{}) {
			data.UpFor = rep.At.Sub(rep.StartedAt).Round(time.Second).String()
		}
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "kafka-shadow-section", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// kshadowStatus maps a report to the tile's dot: down (no state),
// mismatch (a gap anywhere), lag (an active queue behind), else ok.
func kshadowStatus(rep *ks.Report) (string, string) {
	if rep == nil || rep.Kshadow == "down" {
		return "down", "bad"
	}
	for _, r := range rep.Rows {
		if r.Mismatch > 0 || r.Dropped > 0 {
			return "mismatch", "bad"
		}
	}
	if rep.DryRun != nil {
		for _, r := range rep.DryRun.Families {
			if r.Unmatched > 0 || r.Inversions > 0 {
				return "mismatch", "bad"
			}
		}
	}
	for _, r := range rep.Rows {
		if !r.LastAt.IsZero() {
			if d, err := time.ParseDuration(r.Lag); err == nil && d > 5*time.Minute {
				return "lag", "warn"
			}
		}
	}
	return "ok", "good"
}
