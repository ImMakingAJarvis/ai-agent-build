package agentjob

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// StreamKey is the Redis stream key holding one job's live output lines.
func StreamKey(jobID string) string { return "jarvis:job:" + jobID }

// StreamAddr returns the Redis address job output is live-streamed to:
// $JARVIS_REDIS, or the local default. "off" or "none" disables streaming.
func StreamAddr() string {
	if a := os.Getenv("JARVIS_REDIS"); a != "" {
		return a
	}
	return "redis://127.0.0.1:6379"
}

// streamMaxLen bounds each job's stream so finished jobs cannot fill
// Redis; the transcript file keeps the full durable copy.
const streamMaxLen = 20000

// streamTTL is how long a finished job's stream survives for late page
// visits; after that the transcript file is the only copy.
const streamTTL = 24 * time.Hour

// StreamPublisher is an io.Writer that publishes each completed output
// line of the agent to the job's Redis stream, where the dashboard reads
// it for the live job view. Writes never block or fail the agent: lines
// go through a bounded channel to a single publisher goroutine, and a
// full channel or a dead Redis only loses live-stream data.
type StreamPublisher struct {
	client  *redis.Client
	key     string
	lines   chan []byte
	pending []byte
	dead    atomic.Bool
	drops   atomic.Int64
	done    chan struct{}
}

// NewStreamPublisher dials Redis and starts the publisher. It returns
// nil when Redis is unreachable or disabled, so the caller can simply
// skip live streaming; the reason goes to stderr (agent.log).
func NewStreamPublisher(addr, jobID string) *StreamPublisher {
	if addr == "off" || addr == "none" {
		return nil
	}
	opt, err := redis.ParseURL(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-jobs: bad JARVIS_REDIS %q: %v\n", addr, err)
		return nil
	}
	if opt.DialTimeout == 0 {
		opt.DialTimeout = time.Second
	}
	client := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err = client.Ping(ctx).Err()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-jobs: redis %s unreachable (%v); live streaming disabled for this job\n", opt.Addr, err)
		client.Close()
		return nil
	}
	p := &StreamPublisher{
		client: client,
		key:    StreamKey(jobID),
		lines:  make(chan []byte, 4096),
		done:   make(chan struct{}),
	}
	go p.run()
	return p
}

// run XADDs each line until Close drains the channel. A failed XADD
// kills streaming for the rest of the job (one stderr note, no retries).
func (p *StreamPublisher) run() {
	for line := range p.lines {
		if p.dead.Load() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		err := p.client.XAdd(ctx, &redis.XAddArgs{
			Stream:           p.key,
			MaxLen: streamMaxLen,
			Approx: true,
			Values:           map[string]any{"data": string(line), "ts": time.Now().UnixMilli()},
		}).Err()
		cancel()
		if err != nil {
			p.dead.Store(true)
			fmt.Fprintf(os.Stderr, "jarvis-jobs: live stream to redis failed: %v\n", err)
		}
	}
	close(p.done)
}

// Write buffers bytes and publishes every completed line. The trailing
// partial line is kept until its newline arrives (it is always also in
// the transcript file, so a dropped line only hurts the live view).
func (p *StreamPublisher) Write(b []byte) (int, error) {
	p.pending = append(p.pending, b...)
	for {
		i := bytes.IndexByte(p.pending, '\n')
		if i < 0 {
			break
		}
		line := append([]byte(nil), p.pending[:i+1]...)
		select {
		case p.lines <- line:
		default:
			p.drops.Add(1)
		}
		p.pending = p.pending[i+1:]
	}
	// A line longer than 1 MB without a newline is pathological; drop it
	// so pending can never grow unbounded.
	if len(p.pending) > 1<<20 {
		p.drops.Add(1)
		p.pending = nil
	}
	return len(b), nil
}

// Close flushes queued lines and stops the publisher. It must not run
// while Write can still be called (the agent process is already waited
// on by the time Run closes it).
func (p *StreamPublisher) Close() {
	close(p.lines)
	<-p.done
	// Release the stream once the job is over; the transcript file is
	// the durable copy. A failed Expire just means the stream lingers.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	p.client.Expire(ctx, p.key, streamTTL)
	cancel()
	p.client.Close()
}

// Drops reports how many lines were dropped on a full channel.
func (p *StreamPublisher) Drops() int64 { return p.drops.Load() }
