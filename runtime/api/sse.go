// sse.go serves GET /v1/events (card ELR-96, M4.1d): the broker's
// events as server-sent events — `id: <ID>\nevent: <Type>\ndata:
// <json>\n\n`, flushed after each event. The stream ends when the
// client goes away or the broker closes. Every Data passes the Redactor
// (re-marshalled string fields) before it is written (edge case
// 2026-11-24): no endpoint returns raw text.
package api

import (
	"fmt"
	"net/http"
)

// handleEvents streams the broker's events to the client as
// server-sent events. A nil Broker answers 503: card m4-1f builds the
// broker and wires the loop's events (Runner.OnProgress/OnDone, the
// tick end) into Publish, so until then there is nothing to stream.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	b := s.deps.Broker
	if b == nil {
		httpError(w, http.StatusServiceUnavailable, "event stream not wired yet (card m4-1f)")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := b.Subscribe()
	defer cancel()
	for {
		select {
		case <-r.Context().Done():
			return // the client went away.
		case e, ok := <-ch:
			if !ok {
				return // the broker closed.
			}
			data, err := s.redacted(e.Data)
			if err != nil {
				// Data that is not valid JSON is a publisher bug;
				// ending the stream is the safe answer.
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, data); err != nil {
				return // the client went away.
			}
			flusher.Flush()
		}
	}
}
