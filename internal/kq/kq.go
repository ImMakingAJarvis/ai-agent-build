// Package kq is the small Kafka client shared by cmd/jarvis-kq and
// cmd/kafka-exporter (RabbitMQ→Kafka shadow migration,
// docs/kafka-migration-plan.md §4).
//
// segmentio/kafka-go v0.4 removed the legacy convenience Client API, so this
// package uses the raw per-API client methods (Metadata, ListOffsets,
// ListGroups, OffsetFetch, DescribeConfigs, DeleteTopics), *kafka.Conn for
// record reads, and one registered protocol message (DeleteRecords) for
// purges — see deleterecords.go.
package kq

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/segmentio/kafka-go"
)

// Client is a small handle over a Kafka cluster.
type Client struct {
	Brokers []string
	Timeout time.Duration
}

// New returns a Client for the given broker addresses (default
// 127.0.0.1:9092) with a 10s request timeout.
func New(brokers ...string) *Client {
	if len(brokers) == 0 {
		brokers = []string{"127.0.0.1:9092"}
	}
	return &Client{Brokers: brokers, Timeout: 10 * time.Second}
}

// raw is a throwaway kafka.Client for one request; the v0.4 client is a
// plain value, so there is no pool or state to manage here.
func (c *Client) raw() *kafka.Client {
	return &kafka.Client{
		Addr:    kafka.TCP(c.Brokers[0]),
		Timeout: c.Timeout,
	}
}

// Metadata returns cluster metadata for the named topics (all topics when
// the list is empty).
func (c *Client) Metadata(ctx context.Context, topics []string) (*kafka.MetadataResponse, error) {
	return c.raw().Metadata(ctx, &kafka.MetadataRequest{Topics: topics})
}

// TopicInfo is one topic's metadata row.
type TopicInfo struct {
	Name       string
	Partitions []kafka.Partition
	Error      error
}

// Topics lists every topic the broker knows, with partitions.
func (c *Client) Topics(ctx context.Context) ([]TopicInfo, error) {
	md, err := c.Metadata(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]TopicInfo, 0, len(md.Topics))
	for _, t := range md.Topics {
		out = append(out, TopicInfo{Name: t.Name, Partitions: t.Partitions, Error: t.Error})
	}
	return out, nil
}

// offsets fetches one kind of log offset (kafka.FirstOffset or
// kafka.LastOffset) for every partition of a topic.
func (c *Client) offsets(ctx context.Context, topic string, which int64) (map[int]int64, error) {
	md, err := c.Metadata(ctx, []string{topic})
	if err != nil {
		return nil, err
	}
	if len(md.Topics) == 0 {
		return nil, fmt.Errorf("topic %q not found", topic)
	}
	t := md.Topics[0]
	if t.Error != nil {
		return nil, fmt.Errorf("topic %q: %w", topic, t.Error)
	}
	reqs := make([]kafka.OffsetRequest, 0, len(t.Partitions))
	for _, p := range t.Partitions {
		reqs = append(reqs, kafka.OffsetRequest{Partition: p.ID, Timestamp: which})
	}
	res, err := c.raw().ListOffsets(ctx, &kafka.ListOffsetsRequest{
		Topics: map[string][]kafka.OffsetRequest{topic: reqs},
	})
	if err != nil {
		return nil, err
	}
	out := make(map[int]int64, len(reqs))
	for _, po := range res.Topics[topic] {
		if po.Error != nil {
			return nil, fmt.Errorf("partition %d: %w", po.Partition, po.Error)
		}
		if which == kafka.FirstOffset {
			out[po.Partition] = po.FirstOffset
		} else {
			out[po.Partition] = po.LastOffset
		}
	}
	return out, nil
}

// FirstOffsets returns the log-start offset per partition of a topic.
func (c *Client) FirstOffsets(ctx context.Context, topic string) (map[int]int64, error) {
	return c.offsets(ctx, topic, kafka.FirstOffset)
}

// LastOffsets returns the log-end offset per partition of a topic.
func (c *Client) LastOffsets(ctx context.Context, topic string) (map[int]int64, error) {
	return c.offsets(ctx, topic, kafka.LastOffset)
}

// OffsetAt returns, per partition, the offset of the first record at or
// after t (the log-end offset when the topic has nothing that late).
func (c *Client) OffsetAt(ctx context.Context, topic string, at time.Time) (map[int]int64, error) {
	md, err := c.Metadata(ctx, []string{topic})
	if err != nil {
		return nil, err
	}
	if len(md.Topics) == 0 {
		return nil, fmt.Errorf("topic %q not found", topic)
	}
	t := md.Topics[0]
	if t.Error != nil {
		return nil, fmt.Errorf("topic %q: %w", topic, t.Error)
	}
	reqs := make([]kafka.OffsetRequest, 0, len(t.Partitions))
	for _, p := range t.Partitions {
		reqs = append(reqs, kafka.TimeOffsetOf(p.ID, at))
	}
	res, err := c.raw().ListOffsets(ctx, &kafka.ListOffsetsRequest{
		Topics: map[string][]kafka.OffsetRequest{topic: reqs},
	})
	if err != nil {
		return nil, err
	}
	out := make(map[int]int64, len(reqs))
	for _, po := range res.Topics[topic] {
		if po.Error != nil {
			return nil, fmt.Errorf("partition %d: %w", po.Partition, po.Error)
		}
		for off := range po.Offsets {
			out[po.Partition] = off
		}
	}
	// A time query newer than any record comes back as offset -1 (the broker
	// echoes the requested timestamp, error 0), so map those partitions to
	// the log-end offset: "nothing that late" reads as no records.
	var last map[int]int64
	for p, off := range out {
		if off >= 0 {
			continue
		}
		if last == nil {
			var err error
			last, err = c.offsets(ctx, topic, kafka.LastOffset)
			if err != nil {
				return nil, err
			}
		}
		out[p] = last[p]
	}
	return out, nil
}

// Groups lists every consumer group the broker knows.
func (c *Client) Groups(ctx context.Context) ([]kafka.ListGroupsResponseGroup, error) {
	res, err := c.raw().ListGroups(ctx, &kafka.ListGroupsRequest{})
	if err != nil {
		return nil, err
	}
	return res.Groups, nil
}

// GroupOffset returns the group's committed offsets, topic → partitions.
func (c *Client) GroupOffset(ctx context.Context, group string) (map[string][]kafka.OffsetFetchPartition, error) {
	res, err := c.raw().OffsetFetch(ctx, &kafka.OffsetFetchRequest{GroupID: group})
	if err != nil {
		return nil, err
	}
	return res.Topics, nil
}

// TopicConfig returns the topic's effective configuration
// (retention.ms, cleanup.policy, ...).
func (c *Client) TopicConfig(ctx context.Context, topic string) (map[string]string, error) {
	res, err := c.raw().DescribeConfigs(ctx, &kafka.DescribeConfigsRequest{
		Resources: []kafka.DescribeConfigRequestResource{
			{ResourceType: kafka.ResourceTypeTopic, ResourceName: topic},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(res.Resources) != 1 {
		return nil, fmt.Errorf("no config returned for topic %q", topic)
	}
	r := res.Resources[0]
	if r.Error != nil {
		return nil, fmt.Errorf("topic %q: %w", topic, r.Error)
	}
	out := make(map[string]string, len(r.ConfigEntries))
	for _, e := range r.ConfigEntries {
		out[e.ConfigName] = e.ConfigValue
	}
	return out, nil
}

// Send produces one record to the topic (RequireAll) and returns the
// partition it landed on plus the offset of the record. A topic the broker
// does not know is created first (one partition, RF 1): since KIP-527
// (Kafka ≥ 2.4) a metadata request no longer auto-creates a topic, and a
// raw produce to an unknown topic was not reliably created either, so
// creation is explicit (auto.create.topics.enable stays on for external
// tools like console-producer).
func (c *Client) Send(ctx context.Context, topic string, key, value []byte, headers []kafka.Header) (int, int64, error) {
	md, err := c.Metadata(ctx, []string{topic})
	if err != nil {
		return 0, 0, err
	}
	if len(md.Topics) != 1 || md.Topics[0].Error != nil {
		if err := c.CreateTopic(ctx, topic); err != nil {
			return 0, 0, err
		}
		md, err = c.Metadata(ctx, []string{topic})
		if err != nil {
			return 0, 0, err
		}
	}
	t := md.Topics[0]
	if t.Error != nil {
		return 0, 0, t.Error
	}
	partitions := make([]int, len(t.Partitions))
	for i, p := range t.Partitions {
		partitions[i] = p.ID
	}
	msg := kafka.Message{Key: key, Value: value, Headers: headers}
	partition := partitions[0]
	if len(partitions) > 1 {
		partition = int((&kafka.Hash{}).Balance(msg, partitions...))
	}
	w := &kafka.Writer{
		Addr:         kafka.TCP(c.Brokers[0]),
		Topic:        topic,
		RequiredAcks: kafka.RequireAll,
	}
	defer w.Close()
	if err := w.WriteMessages(ctx, msg); err != nil {
		return 0, 0, err
	}
	after, err := c.LastOffsets(ctx, topic)
	if err != nil {
		return 0, 0, err
	}
	// The record we just produced occupies offset after[partition]-1
	// (true while this CLI is the only producer to the partition).
	return partition, after[partition] - 1, nil
}

// CreateTopic creates the topic with one partition and RF 1 (the plan's
// defaults for the static set), tolerating an already-existing topic.
func (c *Client) CreateTopic(ctx context.Context, name string) error {
	res, err := c.raw().CreateTopics(ctx, &kafka.CreateTopicsRequest{
		Topics: []kafka.TopicConfig{{Topic: name, NumPartitions: 1, ReplicationFactor: 1}},
	})
	if err != nil {
		return err
	}
	if err := res.Errors[name]; err != nil && !errors.Is(err, kafka.TopicAlreadyExists) {
		return err
	}
	return nil
}

// Conn dials the partition leader directly (for record reads).
func (c *Client) Conn(ctx context.Context, topic string, partition int) (*kafka.Conn, error) {
	return kafka.DialLeader(ctx, "tcp", c.Brokers[0], topic, partition)
}

// DeleteTopic deletes a topic (requires delete.topic.enable=true).
func (c *Client) DeleteTopic(ctx context.Context, topic string) error {
	res, err := c.raw().DeleteTopics(ctx, &kafka.DeleteTopicsRequest{Topics: []string{topic}})
	if err != nil {
		return err
	}
	if err := res.Errors[topic]; err != nil {
		return err
	}
	return nil
}

// controllerAddr resolves the cluster controller's host:port.
func (c *Client) controllerAddr(ctx context.Context) (string, error) {
	md, err := c.Metadata(ctx, nil)
	if err != nil {
		return "", err
	}
	if md.Controller.ID < 0 || md.Controller.Host == "" {
		return "", fmt.Errorf("controller broker not reported in metadata")
	}
	return net.JoinHostPort(md.Controller.Host, fmt.Sprintf("%d", md.Controller.Port)), nil
}

// Purge deletes every record in the topic (DeleteRecords to the log end,
// per partition) and returns the new log-start offset per partition.
func (c *Client) Purge(ctx context.Context, topic string) (map[int]int64, error) {
	last, err := c.LastOffsets(ctx, topic)
	if err != nil {
		return nil, err
	}
	addr, err := c.controllerAddr(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", addr, c.Timeout)
	if err != nil {
		return nil, fmt.Errorf("controller %s: %w", addr, err)
	}
	defer conn.Close()
	var parts []deleteRecordsRequestPartition
	for p, off := range last {
		parts = append(parts, deleteRecordsRequestPartition{
			PartitionIndex: int32(p),
			Offset:         off,
			Timestamp:      -1,
		})
	}
	req := &deleteRecordsRequest{
		Topics: []deleteRecordsRequestTopic{{Name: topic, Partitions: parts}},
	}
	// DeleteRecords v1; the broker supports v0–v2 on 3.x.
	res, err := protocolRoundTrip(conn, 1, 0, "jarvis-kq", req)
	if err != nil {
		return nil, err
	}
	dr := res.(*deleteRecordsResponse)
	if len(dr.Topics) == 0 {
		return nil, fmt.Errorf("no partition results for %q", topic)
	}
	out := make(map[int]int64, len(dr.Topics[0].Partitions))
	for _, tp := range dr.Topics[0].Partitions {
		if tp.OffsetErrorCode != 0 {
			return nil, fmt.Errorf("partition %d: kafka error %d", tp.PartitionIndex, tp.OffsetErrorCode)
		}
		out[int(tp.PartitionIndex)] = tp.LowWatermark
	}
	return out, nil
}
