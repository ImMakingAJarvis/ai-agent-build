package kshadow

import (
	"context"
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
