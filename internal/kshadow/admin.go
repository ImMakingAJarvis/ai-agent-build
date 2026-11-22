package kshadow

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"
)

// MgmtStats is one queue's counters as the broker reports them
// (cumulative since the last broker restart — parity diffs them against
// kshadow's per-queue baselines, captured when the queue was first seen).
type MgmtStats struct {
	Published int64
	Delivered int64
	Ready     int64
	Unacked   int64
}

// Admin is a minimal client for the RabbitMQ management API: parity needs
// the broker's own publish/deliver counters, which kshadow's tap (and its
// own counters) only see from the mirror's viewpoint.
type Admin struct {
	base   string
	user   string
	pass   string
	vhost  string
	client *http.Client
}

// NewAdmin builds an Admin from the service's AMQP URL (amqp://user:pass@
// host:port[/vhost]) — the management port is 15672 on the same host. A
// direct http(s)://…:15672 URL is used as-is (KSHADOW_RABBITMQ_MGMT).
func NewAdmin(amqpURL string) (*Admin, error) {
	u, err := url.Parse(amqpURL)
	if err != nil {
		return nil, fmt.Errorf("admin: parse %q: %w", amqpURL, err)
	}
	base, user, pass, vhost := u.Scheme, "", "", "/"
	switch u.Scheme {
	case "amqp", "amqps":
		host := u.Hostname()
		if host == "" {
			return nil, fmt.Errorf("admin: no host in %q", amqpURL)
		}
		port := "15672"
		if p := u.Port(); p != "" && u.Scheme == "amqps" {
			port = "15671"
		}
		secure := "http"
		if u.Scheme == "amqps" {
			secure = "https"
		}
		base = secure + "://" + host + ":" + port
		if u.User != nil {
			user = u.User.Username()
			pass, _ = u.User.Password()
		}
		if len(u.Path) > 1 {
			vhost = u.Path
		}
	case "http", "https":
		base = amqpURL
	default:
		return nil, fmt.Errorf("admin: unsupported URL scheme %q", u.Scheme)
	}
	return &Admin{
		base:   base,
		user:   user,
		pass:   pass,
		vhost:  vhost,
		client: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func (a *Admin) get(ctx context.Context, p string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+p, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(a.user, a.pass)
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return fmt.Errorf("admin: GET %s: %s: %s", p, res.Status, string(b))
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// QueueMessage is one message as the management API reports it. Properties
// holds the AMQP properties (message_id, correlation_id, headers, …) as a
// raw map — the rollback path only needs the ids. Payload is the decoded
// body (the API's base64 payload is unmarshalled into it).
type QueueMessage struct {
	Properties      map[string]any `json:"properties"`
	Payload         string         `json:"payload"`
	PayloadEncoding string         `json:"payload_encoding"`
	RoutingKey      string         `json:"routing_key"`
	Exchange        string         `json:"exchange"`
	Redelivered     bool           `json:"redelivered"`
	MessageCount    int64          `json:"message_count"`
}

// GetQueueMessage pops one message from q via the management API
// (POST /api/queues/{v}/{q}/get). On this broker build (3.13.x) the
// endpoint is POST, takes its params in the JSON body, and only supports
// base64 payloads — a GET is 405 and utf8 is rejected. ackmode is
// "reject_requeue_false" (the pop removes it — the rollback's drop) or
// "reject_requeue_true" (it goes back — the dry run). A nil message is an
// empty queue.
func (a *Admin) GetQueueMessage(ctx context.Context, queue, ackmode string) (*QueueMessage, error) {
	body, err := json.Marshal(map[string]any{
		"count":    1,
		"ackmode":  ackmode,
		"encoding": "base64",
	})
	if err != nil {
		return nil, err
	}
	p := path.Join("/api/queues", url.PathEscape(a.vhost), queue, "get")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+p, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(a.user, a.pass)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return nil, fmt.Errorf("admin: POST %s: %s: %s", p, res.Status, string(b))
	}
	var out []QueueMessage
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	m := &out[0]
	if m.PayloadEncoding == "base64" {
		raw, err := base64.StdEncoding.DecodeString(m.Payload)
		if err != nil {
			return nil, fmt.Errorf("admin: decode queue payload: %w", err)
		}
		m.Payload = string(raw)
	}
	return m, nil
}

// MessageID returns the popped message's identity under the same rules as
// the mirror (kshadow.MessageID): message-id property, else body JSON.
func (m *QueueMessage) MessageID() string {
	id, _ := m.Properties["message_id"].(string)
	return MessageID(id, []byte(m.Payload))
}

// PublishToExchange republishes one message through the management API
// (POST /api/exchanges/{v}/{e}/publish) — the rollback's requeue of a
// popped message whose identity could not be classified. payload must
// already be encoded as encoding; this broker build only accepts "base64"
// (utf8/auto are rejected), so callers encode before calling.
func (a *Admin) PublishToExchange(ctx context.Context, exchange, routingKey string, props map[string]any, payload, encoding string) error {
	if encoding == "" {
		encoding = "string"
	}
	body, err := json.Marshal(map[string]any{
		"properties":       props,
		"routing_key":      routingKey,
		"payload":          payload,
		"payload_encoding": encoding,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.base+path.Join("/api/exchanges", url.PathEscape(a.vhost), exchange, "publish"),
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(a.user, a.pass)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNoContent {
		return fmt.Errorf("admin: publish to %s: message was unroutable", exchange)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return fmt.Errorf("admin: publish to %s: %s: %s", exchange, res.Status, string(b))
	}
	return nil
}

// QueueStats returns q's counters from the management API.
func (a *Admin) QueueStats(ctx context.Context, queue string) (MgmtStats, error) {
	var q struct {
		MessageStats struct {
			Publish int64 `json:"publish"`
			Deliver int64 `json:"deliver"`
		} `json:"message_stats"`
		MessagesReady          int64 `json:"messages_ready"`
		MessagesUnacknowledged int64 `json:"messages_unacknowledged"`
	}
	// path.Join would swallow a "/" vhost segment (it cleans empties);
	// the mgmt API wants it URL-encoded in the path.
	p := path.Join("/api/queues", url.PathEscape(a.vhost), queue)
	if err := a.get(ctx, p, &q); err != nil {
		return MgmtStats{}, err
	}
	return MgmtStats{
		Published: q.MessageStats.Publish,
		Delivered: q.MessageStats.Deliver,
		Ready:     q.MessagesReady,
		Unacked:   q.MessagesUnacknowledged,
	}, nil
}
