// kafka.go: the dashboard's Kafka view (plan §4). While both brokers run
// the queues page reads the Kafka Admin API (topics/depth/lag) alongside
// the AMQP view; the history section gives a per-topic record table with
// click-through to the payload; the dead-letters section also lists the
// `dead` topic (with requeue/drop on the record). The topic snapshot is
// cached 20s server-side: ListOffsets per topic is the cost, and the tile
// is a health view, not a live counter.
package dashboard

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kq"
	"github.com/segmentio/kafka-go"
)

// renderSection executes a named template section into the response (the
// shared shape of the htmx section handlers).
func (s *Server) renderSection(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		fmt.Fprintf(os.Stderr, "dashboard: render %s: %v\n", name, err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

type kafkaTopicRow struct {
	Name  string `json:"name"`
	First int64  `json:"first"`
	Last  int64  `json:"last"`
	Depth int64  `json:"depth"`
}

type kafkaGroupRow struct {
	Group string `json:"group"`
	Lag   int64  `json:"lag"`
}

type kafkaSnapshot struct {
	Topics []kafkaTopicRow `json:"topics"`
	Groups []kafkaGroupRow `json:"groups"`
	At     time.Time       `json:"at"`
}

// kafkaClient returns a fresh kq client (the dashboard holds no long-lived
// Kafka connection, like the AMQP side).
func (s *Server) kafkaClient() *kq.Client { return kq.New(s.kafkaBrokers...) }

func sum64(m map[int]int64) int64 {
	var sum int64
	for _, v := range m {
		sum += v
	}
	return sum
}

// kafkaSnapshot builds (or serves the fresh cache of) the topic/group view.
func (s *Server) kafkaSnapshot() (kafkaSnapshot, error) {
	s.kafkaMu.Lock()
	if !s.kafkaSnapAt.IsZero() && time.Since(s.kafkaSnapAt) < 20*time.Second {
		snap := s.kafkaSnap
		s.kafkaMu.Unlock()
		return snap, nil
	}
	s.kafkaMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := s.kafkaClient()
	topics, err := c.Topics(ctx)
	if err != nil {
		return kafkaSnapshot{}, err
	}
	last := make(map[string]map[int]int64, len(topics))
	rows := make([]kafkaTopicRow, 0, len(topics))
	for _, t := range topics {
		if t.Error != nil || len(t.Name) > 0 && t.Name[0] == '_' {
			continue // broker-internal topics
		}
		f, ferr := c.FirstOffsets(ctx, t.Name)
		l, lerr := c.LastOffsets(ctx, t.Name)
		if ferr != nil || lerr != nil {
			continue
		}
		last[t.Name] = l
		first, end := sum64(f), sum64(l)
		rows = append(rows, kafkaTopicRow{Name: t.Name, First: first, Last: end, Depth: end - first})
	}
	groups, gerr := c.Groups(ctx)
	if gerr != nil {
		groups = nil // groups are a bonus; topic failure already reported
	}
	grows := make([]kafkaGroupRow, 0, len(groups))
	for _, g := range groups {
		var lag int64
		if off, oerr := c.GroupOffset(ctx, g.GroupID); oerr == nil {
			for topic, parts := range off {
				ends, ok := last[topic]
				if !ok {
					continue
				}
				for _, p := range parts {
					if p.Error != nil {
						continue
					}
					if end, ok := ends[p.Partition]; ok {
						lag += end - p.CommittedOffset
					}
				}
			}
		}
		grows = append(grows, kafkaGroupRow{Group: g.GroupID, Lag: lag})
	}
	snap := kafkaSnapshot{Topics: rows, Groups: grows, At: time.Now().UTC()}
	s.kafkaMu.Lock()
	s.kafkaSnap, s.kafkaSnapAt = snap, time.Now()
	s.kafkaMu.Unlock()
	return snap, nil
}

func (s *Server) handleKafkaQueuesHTML(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Err": "", "Topics": []kafkaTopicRow{}, "Groups": []kafkaGroupRow{}}
	snap, err := s.kafkaSnapshot()
	if err != nil {
		data["Err"] = err.Error()
	} else {
		data["Topics"], data["Groups"], data["At"] = snap.Topics, snap.Groups, snap.At
	}
	s.renderSection(w, "kafka-queues-section", data)
}

func (s *Server) handleKafkaHistoryHTML(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Err": "", "Topics": []string{}}
	snap, err := s.kafkaSnapshot()
	if err != nil {
		data["Err"] = err.Error()
	} else {
		for _, t := range snap.Topics {
			data["Topics"] = append(data["Topics"].([]string), t.Name)
		}
	}
	s.renderSection(w, "kafka-history-section", data)
}

// handleKafkaTopicsAPI is the JSON twin of the queues tile.
func (s *Server) handleKafkaTopicsAPI(w http.ResponseWriter, r *http.Request) {
	snap, err := s.kafkaSnapshot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	respondJSON(w, snap)
}

type kafkaRecordOut struct {
	Offset  int64             `json:"offset"`
	Time    string            `json:"time"`
	Key     string            `json:"key"`
	Headers map[string]string `json:"headers"`
	Value   string            `json:"value"`
}

func recordOut(m kafka.Message) kafkaRecordOut {
	hdrs := map[string]string{}
	for _, h := range m.Headers {
		hdrs[h.Key] = string(h.Value)
	}
	return kafkaRecordOut{
		Offset:  m.Offset,
		Time:    m.Time.UTC().Format(time.RFC3339),
		Key:     string(m.Key),
		Headers: hdrs,
		Value:    string(m.Value),
	}
}

// handleKafkaHistoryAPI: the last n records of a topic (newest first).
func (s *Server) handleKafkaHistoryAPI(w http.ResponseWriter, r *http.Request) {
	topic := r.URL.Query().Get("topic")
	if topic == "" {
		http.Error(w, "topic is required", http.StatusBadRequest)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 200 {
		n = 20
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c := s.kafkaClient()
	last, err := c.LastOffsets(ctx, topic)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	end := last[0]
	from := end - int64(n)
	if from < 0 {
		from = 0
	}
	msgs, err := c.ReadRange(ctx, topic, 0, from, end, n)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out := make([]kafkaRecordOut, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		out = append(out, recordOut(msgs[i]))
	}
	respondJSON(w, out)
}

// handleKafkaRecordAPI: one record (the click-through payload view).
func (s *Server) handleKafkaRecordAPI(w http.ResponseWriter, r *http.Request) {
	topic := r.URL.Query().Get("topic")
	off, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if topic == "" || err != nil {
		http.Error(w, "topic and offset are required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	m, err := s.kafkaClient().ReadAt(ctx, topic, 0, off)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	respondJSON(w, recordOut(*m))
}

type kafkaDeadOut struct {
	Offset   int64  `json:"offset"`
	Reason   string `json:"reason"`
	Queue    string `json:"queue"`
	Attempts int    `json:"attempts"`
	At       string `json:"at"`
	Time     string `json:"time"`
	Key      string `json:"key"`
	Value    string `json:"value"`
}

// handleKafkaDead lists the `dead` topic (dropped records hidden).
func (s *Server) handleKafkaDead(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	items, err := s.kafkaClient().ListDead(ctx, 50, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out := make([]kafkaDeadOut, 0, len(items))
	for _, d := range items {
		out = append(out, kafkaDeadOut{
			Offset: d.Offset, Reason: d.Reason, Queue: d.Queue, Attempts: d.Attempts,
			At: d.At, Time: d.Time.UTC().Format(time.RFC3339), Key: d.Key, Value: d.Value,
		})
	}
	respondJSON(w, out)
}

func (s *Server) handleKafkaDeadAction(w http.ResponseWriter, r *http.Request, action string) {
	offset, err := strconv.ParseInt(r.PathValue("offset"), 10, 64)
	if err != nil {
		http.Error(w, "offset must be numeric", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c := s.kafkaClient()
	var done string
	switch action {
	case "requeue":
		queue, p, off, err := c.RequeueDead(ctx, offset)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		done = "requeued to " + queue + " p" + strconv.Itoa(p) + "/" + strconv.FormatInt(off, 10)
	case "drop":
		if err := c.DropDead(ctx, offset); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		done = "dropped"
	default:
		http.Error(w, "unknown action "+action, http.StatusBadRequest)
		return
	}
	w.Write([]byte(done))
}

func (s *Server) handleKafkaDeadRequeue(w http.ResponseWriter, r *http.Request) {
	s.handleKafkaDeadAction(w, r, "requeue")
}

func (s *Server) handleKafkaDeadDrop(w http.ResponseWriter, r *http.Request) {
	s.handleKafkaDeadAction(w, r, "drop")
}
