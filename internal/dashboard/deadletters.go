// Dead letters (EVB-10): messages that a consumer rejected, that expired,
// or that exhausted the redelivery budget sit on the dead queue. This is
// the dashboard side of "nothing disappears silently" — the section lists
// each one (original queue, routing key, reason, time) and can requeue it
// back to its original queue or drop it.
package dashboard

import (
	"fmt"
	"net/http"
)

func (s *Server) handleDeadLetters(w http.ResponseWriter, r *http.Request) {
	if s.rabbitConn == nil || s.rabbitConn.IsClosed() {
		http.Error(w, "rabbitmq not connected", http.StatusBadGateway)
		return
	}
	items, err := s.rabbitConn.ListDead(50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	respondJSON(w, items)
}

func (s *Server) handleDeadLetterAction(w http.ResponseWriter, r *http.Request, action string) {
	if s.rabbitConn == nil || s.rabbitConn.IsClosed() {
		http.Error(w, "rabbitmq not connected", http.StatusBadGateway)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing dead-letter id", http.StatusBadRequest)
		return
	}
	var err error
	switch action {
	case "requeue":
		err = s.rabbitConn.RequeueDead(id)
	case "drop":
		err = s.rabbitConn.DropDead(id)
	default:
		http.Error(w, "unknown action "+action, http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	done := "dropped"
	if action == "requeue" {
		done = "requeued"
	}
	fmt.Fprint(w, done)
}

func (s *Server) handleDeadLetterRequeue(w http.ResponseWriter, r *http.Request) {
	s.handleDeadLetterAction(w, r, "requeue")
}

func (s *Server) handleDeadLetterDrop(w http.ResponseWriter, r *http.Request) {
	s.handleDeadLetterAction(w, r, "drop")
}
