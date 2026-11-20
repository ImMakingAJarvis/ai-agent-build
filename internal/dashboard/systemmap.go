package dashboard

// /map is the interactive system map: every spec in specs/ is a node, every
// depends_on entry an edge. The specs are embedded in the binary (package
// specs), so the map always matches the commit that was deployed. Live
// status comes from /api/map/health, which probes each spec's `health`
// target from inside this container.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/specs"
)

// mapEdge is one dependency on the map: From needs To.
type mapEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Why  string `json:"why"`
}

// mapData is the /api/map payload.
type mapData struct {
	Groups   []string     `json:"groups"`
	Kinds    []string     `json:"kinds"`
	Statuses []string     `json:"statuses"`
	Nodes    []specs.Spec `json:"nodes"`
	Edges    []mapEdge    `json:"edges"`
	Repo     string       `json:"repo"`
	Problems []string     `json:"problems,omitempty"`
}

// mapRepoURL is where the map's "spec source" and doc links point.
const mapRepoURL = "https://github.com/ImMakingAJarvis/ai-agent-build/blob/main/"

var (
	mapOnce   sync.Once
	mapCached mapData
)

// buildMapData parses the embedded specs once (they cannot change while the
// binary runs). Parse or validation problems are reported in the payload
// rather than failing the page — the map still shows what did parse.
func buildMapData() mapData {
	mapOnce.Do(func() {
		all, err := specs.All()
		d := mapData{
			Groups:   specs.Groups,
			Kinds:    specs.Kinds,
			Statuses: specs.Statuses,
			Nodes:    all,
			Repo:     mapRepoURL,
		}
		if err != nil {
			d.Problems = append(d.Problems, err.Error())
		}
		for _, e := range specs.Validate(all) {
			d.Problems = append(d.Problems, e.Error())
		}
		ids := map[string]bool{}
		for _, s := range all {
			ids[s.ID] = true
		}
		for _, s := range all {
			for _, e := range s.DependsOn {
				if ids[e.To] {
					d.Edges = append(d.Edges, mapEdge{From: s.ID, To: e.To, Why: e.Why})
				}
			}
		}
		if d.Nodes == nil {
			d.Nodes = []specs.Spec{}
		}
		mapCached = d
	})
	return mapCached
}

func (s *Server) handleMapPage(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "map.html", nil); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) handleMapAPI(w http.ResponseWriter, r *http.Request) {
	writeMapJSON(w, buildMapData())
}

// handleMapSpec serves one spec's Markdown source (GET /api/map/spec/{id}).
func (s *Server) handleMapSpec(w http.ResponseWriter, r *http.Request) {
	b, err := specs.Source(r.PathValue("id"))
	if err != nil {
		http.Error(w, "no such spec", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b)
}

// probeResult is one node's live status.
type probeResult struct {
	State  string `json:"state"` // up | down
	Detail string `json:"detail"`
	Ms     int64  `json:"ms"`
}

// mapHealth is the /api/map/health payload.
type mapHealth struct {
	CheckedAt string                 `json:"checked_at"`
	Results   map[string]probeResult `json:"results"`
}

const (
	mapProbeTimeout = 2 * time.Second
	mapHealthTTL    = 10 * time.Second
)

// mapProbe checks one target; a variable so tests can replace it.
var mapProbe = probeTarget

var (
	mapHealthMu   sync.Mutex
	mapHealthAt   time.Time
	mapHealthLast mapHealth
)

// probeTarget probes http://... (2xx/3xx = up) or tcp://host:port (a
// connection = up).
func probeTarget(ctx context.Context, target string) probeResult {
	start := time.Now()
	ms := func() int64 { return time.Since(start).Milliseconds() }
	if hostport, ok := strings.CutPrefix(target, "tcp://"); ok {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", hostport)
		if err != nil {
			return probeResult{State: "down", Detail: shortErr(err), Ms: ms()}
		}
		c.Close()
		return probeResult{State: "up", Detail: "tcp connect ok", Ms: ms()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return probeResult{State: "down", Detail: shortErr(err), Ms: ms()}
	}
	client := &http.Client{
		// A redirect is an answer: do not follow it off-box.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return probeResult{State: "down", Detail: shortErr(err), Ms: ms()}
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return probeResult{State: "up", Detail: fmt.Sprintf("HTTP %d", resp.StatusCode), Ms: ms()}
	}
	return probeResult{State: "down", Detail: fmt.Sprintf("HTTP %d", resp.StatusCode), Ms: ms()}
}

// shortErr trims a net error to its last, most specific part.
func shortErr(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		msg = msg[i+2:]
	}
	return msg
}

// handleMapHealth probes every spec that names a health target, in
// parallel, each with a short timeout. Results are cached briefly so
// several open map tabs do not multiply the probes.
func (s *Server) handleMapHealth(w http.ResponseWriter, r *http.Request) {
	mapHealthMu.Lock()
	defer mapHealthMu.Unlock()
	if time.Since(mapHealthAt) < mapHealthTTL && mapHealthLast.Results != nil {
		writeMapJSON(w, mapHealthLast)
		return
	}
	nodes := buildMapData().Nodes
	results := make(map[string]probeResult)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range nodes {
		if n.Health == "" {
			continue
		}
		wg.Add(1)
		go func(id, target string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), mapProbeTimeout)
			defer cancel()
			res := mapProbe(ctx, target)
			mu.Lock()
			results[id] = res
			mu.Unlock()
		}(n.ID, n.Health)
	}
	wg.Wait()
	mapHealthLast = mapHealth{CheckedAt: time.Now().UTC().Format(time.RFC3339), Results: results}
	mapHealthAt = time.Now()
	writeMapJSON(w, mapHealthLast)
}

func writeMapJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
