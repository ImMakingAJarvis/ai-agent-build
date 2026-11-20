// Package main is the five-minute check (Job 10 of
// docs/pr-review-build-plan.md): every five minutes it walks the review
// records, revives whatever next job is missing — publishing the review job
// the same way Job 4 does, or adding the author job to the agent job store —
// and at 18:00 Los Angeles it sends the end of day digest to Discord and
// Plane.
//
// This process is not a model: it never runs claude, agent-local, or
// ollama. It publishes jobs and writes logs; the models run where they
// always have.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/chat"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewcfg"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewstate"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewturn"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewsweep"
)

// passInterval is how long the loop sleeps between passes: the sweep is the
// five-minute check, and the table's waiting threshold is five minutes.
const passInterval = 5 * time.Minute

// suppressWindow counts a revival logged within this many minutes as the
// job being queued: the review worker does not save the record after
// reviewing, so the record's mtime alone would make the sweep republish
// every pass. Fifteen minutes covers the three passes until the worker is
// done and the record moves on.
const suppressWindow = 15 * time.Minute

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)
	dir := flag.String("dir", reviewstate.DefaultDir(), "reviewstate dir (records and the revival log)")
	cfg := flag.String("cfg", envOr("JARVIS_REVIEW_CFG", "config/review.yaml"), "review config")
	data := flag.String("data", agentjob.DataDir(), "agent job data dir (author jobs land here)")
	once := flag.Bool("once", false, "run one pass and exit (no loop, no sleep)")
	flag.Parse()

	rcfg, err := reviewcfg.Load(*cfg)
	if err != nil {
		log.Fatalf("review-sweep: review config: %v", err)
	}
	qc, err := queue.NewConn()
	if err != nil {
		log.Fatalf("review-sweep: broker: %v", err)
	}
	defer qc.Close()
	store, err := agentjob.Open(*data)
	if err != nil {
		log.Fatalf("review-sweep: agent job store: %v", err)
	}

	sweep := reviewsweep.Sweep{
		Dir:  *dir,
		Cfg:  rcfg,
		Now:  time.Now,
		Publish: func(ctx context.Context, m queue.Message, q string) error {
			return qc.Publish(m, q)
		},
		EnqueueAuthor: func(ctx context.Context, r reviewstate.Record) error {
			if _, err := store.Ingest(reviewturn.AuthorJob(r)); err != nil {
				return err
			}
			return nil
		},
		Status: func(pr int) reviewsweep.Status {
			return statusFor(store, *dir, pr, time.Now())
		},
	}

	var sender reviewsweep.DigestSender
	if d, err := newDigestSender(); err != nil {
		log.Printf("review-sweep: end of day digest disabled: %v", err)
	} else {
		sender = d
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		if err := sweep.Once(ctx); err != nil {
			log.Fatalf("review-sweep: pass: %v", err)
		}
		if sender != nil {
			if err := sweep.EndOfDay(ctx, sender); err != nil {
				log.Fatalf("review-sweep: end of day: %v", err)
			}
		}
		return
	}

	ticker := time.NewTicker(passInterval)
	defer ticker.Stop()
	for {
		if err := sweep.Once(ctx); err != nil {
			log.Printf("review-sweep: pass: %v", err)
		}
		if sender != nil {
			if err := sweep.EndOfDay(ctx, sender); err != nil {
				log.Printf("review-sweep: end of day: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			log.Println("review-sweep: stopping")
			return
		case <-ticker.C:
		}
	}
}

// statusFor reports what the sweep can see about a PR's jobs: a revival it
// logged within the suppression window counts as queued (see
// suppressWindow), a matching job in the agent job store counts as queued
// or running by its status, and Waiting is how long the record has sat
// untouched.
func statusFor(store *agentjob.Store, dir string, pr int, now time.Time) reviewsweep.Status {
	st := reviewsweep.Status{}
	revs, err := reviewstate.RevivalsOn(dir, now)
	if err != nil {
		log.Printf("review-sweep: revival log: %v", err)
	} else {
		for _, rv := range revs {
			if rv.PR == pr && now.Sub(rv.At) <= suppressWindow {
				st.JobQueued = true
			}
		}
	}
	jobs, err := store.List()
	if err != nil {
		log.Printf("review-sweep: agent job store: %v", err)
		return st
	}
	for _, j := range jobs {
		if !mentionsPR(j.Title, pr) {
			continue
		}
		switch j.Status {
		case agentjob.Running, agentjob.Claimed:
			st.JobRunning = true
		case agentjob.Ready, agentjob.Waiting, agentjob.Parked, agentjob.Paused:
			st.JobQueued = true
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, fmt.Sprintf("pr-%d.json", pr))); err == nil {
		st.Waiting = now.Sub(fi.ModTime())
	}
	return st
}

// mentionsPR reports whether a job title is a job for PR pr: the author job
// ("PR #268: respond to the reviewer's commits") or the review job
// ("review PR #268").
func mentionsPR(title string, pr int) bool {
	return strings.Contains(title, fmt.Sprintf("PR #%d:", pr)) ||
		strings.Contains(title, fmt.Sprintf("review PR #%d", pr))
}

// digestSender is the production DigestSender: one Discord DM and one Plane
// issue in the review project (REVIEW_PLANE_PROJECT, default REV).
type digestSender struct {
	discord *chat.Discord
	plane   *plane.Client
	project string
}

func (d *digestSender) SendDiscord(ctx context.Context, text string) error {
	return d.discord.Notify(ctx, text)
}

func (d *digestSender) CreateIssue(ctx context.Context, title, body string) (string, error) {
	p, err := plane.ProjectByRef(ctx, d.plane, d.project)
	if err != nil {
		return "", err
	}
	wi, err := d.plane.CreateWorkItem(ctx, p.ID, plane.WorkItemCreate{
		Name:            title,
		DescriptionHTML: escapeHTML(body),
	})
	if err != nil {
		return "", err
	}
	return p.Identifier + "-" + strconv.Itoa(wi.SequenceID), nil
}

func newDigestSender() (reviewsweep.DigestSender, error) {
	discord, err := chat.NewDiscord(chat.Config{
		Token:   os.Getenv("DISCORD_BOT_TOKEN"),
		OwnerID: os.Getenv("DISCORD_OWNER_ID"),
	})
	if err != nil {
		return nil, fmt.Errorf("discord: %w", err)
	}
	p, err := plane.FromEnv()
	if err != nil {
		return nil, fmt.Errorf("plane: %w", err)
	}
	return &digestSender{
		discord: discord,
		plane:   p,
		project: envOr("REVIEW_PLANE_PROJECT", "REV"),
	}, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// escapeHTML escapes the characters with meaning in Plane's
// description_html. The digest body is the sweep's own text (PR numbers,
// states, queue names), but the issue must not interpret it. (net/html is
// not in this box's Go install, so the replacer is local.)
func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
