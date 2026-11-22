// kafka-exporter serves Kafka broker metrics to Prometheus on :9308
// (docs/kafka-migration-plan.md phase 0: "exporter metrics land in
// Prometheus"). It is in-repo because the usual exporter images
// (danielmtaylor/kafka-exporter, dpkp, landoop) cannot be pulled on this
// box; the metric names follow the conventional kafka_exporter so a
// dashboard written for it works.
//
// Metrics are refreshed in the background (POLL_INTERVAL, default 15s) and
// served from cache, so a scrape never waits on the broker.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/kq"
)

type topicDepth struct {
	topic     string
	partition int
	first     int64
	last      int64
}

type groupLag struct {
	group     string
	topic     string
	partition int
	committed int64
	last      int64
}

// snapshot is one refresh's worth of broker state.
type snapshot struct {
	cluster   string
	brokers   int
	topics    []topicDepth
	groups    []groupLag
	groupCount int
	updatedAt time.Time
	err       error
}

type kafkaCollector struct {
	mu       sync.RWMutex
	snap     *snapshot
	c        *kq.Client
	interval time.Duration
}

// Describe implements prometheus.Collector (fixed metric set).
func (k *kafkaCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("kafka_brokers", "Number of brokers in the cluster.", nil, nil)
	ch <- prometheus.NewDesc("kafka_topic_partitions", "Number of partitions of a topic.", []string{"topic"}, nil)
	ch <- prometheus.NewDesc("kafka_topic_partition_offset", "Log-end offset of a topic partition.", []string{"topic", "partition"}, nil)
	ch <- prometheus.NewDesc("kafka_topic_partition_earliest_offset", "Log-start offset of a topic partition.", []string{"topic", "partition"}, nil)
	ch <- prometheus.NewDesc("kafka_topic_partition_depth", "Number of records in a topic partition.", []string{"topic", "partition"}, nil)
	ch <- prometheus.NewDesc("kafka_consumergroups", "Number of consumer groups.", nil, nil)
	ch <- prometheus.NewDesc("kafka_consumergroup_offset", "Committed offset of a consumer group on a partition.", []string{"group", "topic", "partition"}, nil)
	ch <- prometheus.NewDesc("kafka_consumergroup_lag", "Committed-vs-log-end lag of a consumer group on a partition.", []string{"group", "topic", "partition"}, nil)
}

// Collect serves the cached snapshot.
func (k *kafkaCollector) Collect(ch chan<- prometheus.Metric) {
	k.mu.RLock()
	s := k.snap
	k.mu.RUnlock()
	if s == nil {
		return
	}
	if s.err != nil {
		return // stale-but-honest: keep the last good numbers, health endpoint reports the error
	}
	ch <- gauge("kafka_brokers", "Number of brokers in the cluster.", nil, float64(s.brokers), nil)
	partitions := map[string]int{}
	for _, t := range s.topics {
		partitions[t.topic]++
	}
	for t := range partitions {
		ch <- gauge("kafka_topic_partitions", "Number of partitions of a topic.", []string{"topic"}, float64(partitions[t]), map[string]string{"topic": t})
	}
	for _, t := range s.topics {
		ls := map[string]string{"topic": t.topic, "partition": strconv.Itoa(t.partition)}
		tp := []string{"topic", "partition"}
		ch <- gauge("kafka_topic_partition_offset", "Log-end offset of a topic partition.", tp, float64(t.last), ls)
		ch <- gauge("kafka_topic_partition_earliest_offset", "Log-start offset of a topic partition.", tp, float64(t.first), ls)
		ch <- gauge("kafka_topic_partition_depth", "Number of records in a topic partition.", tp, float64(t.last-t.first), ls)
	}
	ch <- gauge("kafka_consumergroups", "Number of consumer groups.", nil, float64(s.groupCount), nil)
	for _, g := range s.groups {
		ls := map[string]string{"group": g.group, "topic": g.topic, "partition": strconv.Itoa(g.partition)}
		gt := []string{"group", "topic", "partition"}
		ch <- gauge("kafka_consumergroup_offset", "Committed offset of a consumer group on a partition.", gt, float64(g.committed), ls)
		ch <- gauge("kafka_consumergroup_lag", "Committed-vs-log-end lag of a consumer group on a partition.", gt, float64(g.last-g.committed), ls)
	}
}

// gauge builds one labeled const metric; labels must be in a stable order
// (MustNewConstMetric takes the values in desc-label order, not pairs).
func gauge(name, help string, labels []string, value float64, labelValues map[string]string) prometheus.Metric {
	desc := prometheus.NewDesc(name, help, labels, nil)
	vals := make([]string, 0, len(labels))
	for _, l := range labels {
		vals = append(vals, labelValues[l])
	}
	return prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, vals...)
}

// refresh queries the broker and stores a new snapshot.
func (k *kafkaCollector) refresh(ctx context.Context) {
	s := &snapshot{}
	md, err := k.c.Metadata(ctx, nil)
	if err != nil {
		s.err = err
		k.store(s)
		return
	}
	s.cluster = md.ClusterID
	s.brokers = len(md.Brokers)
	topics := make([]string, 0, len(md.Topics))
	for _, t := range md.Topics {
		topics = append(topics, t.Name)
	}
	sort.Strings(topics)
	for _, name := range topics {
		first, err := k.c.FirstOffsets(ctx, name)
		if err != nil {
			continue
		}
		last, err := k.c.LastOffsets(ctx, name)
		if err != nil {
			continue
		}
		for p := range first {
			s.topics = append(s.topics, topicDepth{name, p, first[p], last[p]})
		}
	}
	sort.Slice(s.topics, func(i, j int) bool {
		if s.topics[i].topic != s.topics[j].topic {
			return s.topics[i].topic < s.topics[j].topic
		}
		return s.topics[i].partition < s.topics[j].partition
	})
	groups, err := k.c.Groups(ctx)
	if err != nil {
		s.err = fmt.Errorf("groups: %w", err)
		k.store(s)
		return
	}
	s.groupCount = len(groups)
	sort.Slice(groups, func(i, j int) bool { return groups[i].GroupID < groups[j].GroupID })
	for _, g := range groups {
		committed, err := k.c.GroupOffset(ctx, g.GroupID)
		if err != nil {
			continue
		}
		lastCache := map[string]map[int]int64{}
		for topic, parts := range committed {
			last, ok := lastCache[topic]
			if !ok {
				last, err = k.c.LastOffsets(ctx, topic)
				if err != nil {
					continue
				}
				lastCache[topic] = last
			}
			for _, po := range parts {
				s.groups = append(s.groups, groupLag{
					group:     g.GroupID,
					topic:     topic,
					partition: po.Partition,
					committed: po.CommittedOffset,
					last:      last[po.Partition],
				})
			}
		}
	}
	s.updatedAt = time.Now()
	k.store(s)
}

func (k *kafkaCollector) store(s *snapshot) {
	k.mu.Lock()
	k.snap = s
	k.mu.Unlock()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	brokers := strings.Split(envOr("KAFKA_BROKERS", "127.0.0.1:9092"), ",")
	listen := envOr("LISTEN_ADDR", ":9308")
	interval, err := time.ParseDuration(envOr("POLL_INTERVAL", "15s"))
	if err != nil || interval < time.Second {
		interval = 15 * time.Second
	}
	c := kq.New(brokers...)
	collector := &kafkaCollector{c: c, interval: interval}
	prometheus.MustRegister(collector)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// First refresh before serving so /metrics is not empty.
	collector.refresh(ctx)
	ticker := time.NewTicker(collector.interval)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				collector.refresh(ctx)
			}
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		rctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := c.Topics(rctx); err != nil {
			http.Error(w, fmt.Sprintf("kafka: %v", err), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{Addr: listen, Handler: mux}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("kafka-exporter listening on %s (brokers %s, refresh %s)", listen, brokers, interval)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		log.Printf("kafka-exporter: %v", err)
		os.Exit(1)
	}
}
