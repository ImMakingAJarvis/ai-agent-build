package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
)

// ingestAgentJob adds a ready agent job to the test store.
func ingestAgentJob(t *testing.T, id, class string, priority int, deps []string) {
	t.Helper()
	st, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Ingest(agentjob.Spec{ID: id, Title: "T-" + id, Class: class, Priority: priority, DependsOn: deps}); err != nil {
		t.Fatal(err)
	}
}

func setAgentStatus(t *testing.T, id, status string) {
	t.Helper()
	st, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus(id, agentjob.State{Status: status}); err != nil {
		t.Fatal(err)
	}
}

func getQueue(t *testing.T, s *Server) queueResponse {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleQueue(w, httptest.NewRequest("GET", "/api/queue", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/queue: %d %s", w.Code, w.Body.String())
	}
	var r queueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return r
}

func postReorder(t *testing.T, s *Server, orders map[string][]string) (*httptest.ResponseRecorder, queueResponse) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"orders": orders})
	req := httptest.NewRequest("POST", "/api/queue/reorder", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleQueueReorder(w, req)
	var r queueResponse
	if w.Code == http.StatusOK {
		json.Unmarshal(w.Body.Bytes(), &r)
	}
	return w, r
}

func postBandOrder(t *testing.T, s *Server, order []string) (*httptest.ResponseRecorder, queueResponse) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"order": order})
	req := httptest.NewRequest("POST", "/api/queue/band-order", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleQueueBandOrder(w, req)
	var r queueResponse
	if w.Code == http.StatusOK {
		json.Unmarshal(w.Body.Bytes(), &r)
	}
	return w, r
}

// bandIDs returns the job ids of one named band, in order.
func bandIDs(r queueResponse, name string) []string {
	for _, b := range r.Bands {
		if b.Name == name {
			var ids []string
			for _, j := range b.Jobs {
				ids = append(ids, j.ID)
			}
			return ids
		}
	}
	return nil
}

func TestQueueBandsDefaultOrder(t *testing.T) {
	s := newTestServer(t)
	ingestAgentJob(t, "b1", agentjob.ClassBuild, 1, nil)
	ingestAgentJob(t, "b2", agentjob.ClassBuild, 9, nil)
	ingestAgentJob(t, "g1", agentjob.ClassBackground, 5, nil)
	ingestAgentJob(t, "done1", agentjob.ClassBuild, 50, nil)
	setAgentStatus(t, "done1", agentjob.Done)

	r := getQueue(t, s)
	if len(r.Bands) != 3 ||
		r.Bands[0].Name != agentjob.ClassFix ||
		r.Bands[1].Name != agentjob.ClassBuild ||
		r.Bands[2].Name != agentjob.ClassBackground {
		t.Fatalf("bands out of the default band order: %+v", r.Bands)
	}
	// Within build, higher priority first: b2 then b1. done1 is not ready.
	if got := bandIDs(r, agentjob.ClassBuild); strings.Join(got, ",") != "b2,b1" {
		t.Fatalf("build band = %v, want [b2 b1]", got)
	}
	if got := bandIDs(r, agentjob.ClassBackground); strings.Join(got, ",") != "g1" {
		t.Fatalf("background band = %v, want [g1]", got)
	}
}

func TestQueueReorderAndReclassify(t *testing.T) {
	s := newTestServer(t)
	ingestAgentJob(t, "b1", agentjob.ClassBuild, 1, nil)
	ingestAgentJob(t, "b2", agentjob.ClassBuild, 9, nil)
	ingestAgentJob(t, "g1", agentjob.ClassBackground, 5, nil)

	// Promote background job g1 to the top of the build band, push b1 above b2.
	w, r := postReorder(t, s, map[string][]string{
		agentjob.ClassBuild:      {"g1", "b1", "b2"},
		agentjob.ClassBackground: {},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("reorder: %d %s", w.Code, w.Body.String())
	}
	if got := bandIDs(r, agentjob.ClassBuild); strings.Join(got, ",") != "g1,b1,b2" {
		t.Fatalf("build band = %v, want [g1 b1 b2]", got)
	}
	if got := bandIDs(r, agentjob.ClassBackground); len(got) != 0 {
		t.Fatalf("background band = %v, want empty", got)
	}

	// The reclassification persisted: g1 is now a build job and the run order
	// (via the store) follows the new arrangement.
	st, _ := agentjob.Open(agentjob.DataDir())
	if g1, _ := st.Get("g1"); g1.Class != agentjob.ClassBuild {
		t.Fatalf("g1 class %q, want %q", g1.Class, agentjob.ClassBuild)
	}
	var got []string
	for {
		j, err := st.Next()
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			break
		}
		got = append(got, j.ID)
		st.SetStatus(j.ID, agentjob.State{Status: agentjob.Skipped})
	}
	if strings.Join(got, ",") != "g1,b1,b2" {
		t.Fatalf("run order %v, want [g1 b1 b2]", got)
	}
}

func TestQueueReorderValidation(t *testing.T) {
	s := newTestServer(t)
	ingestAgentJob(t, "b1", agentjob.ClassBuild, 1, nil)
	ingestAgentJob(t, "b2", agentjob.ClassBuild, 2, nil)
	ingestAgentJob(t, "done1", agentjob.ClassBuild, 5, nil)
	setAgentStatus(t, "done1", agentjob.Done)

	// A ready job left out of the request.
	if w, _ := postReorder(t, s, map[string][]string{agentjob.ClassBuild: {"b1"}}); w.Code != http.StatusConflict {
		t.Fatalf("missing job: status %d", w.Code)
	}
	// A job listed twice.
	if w, _ := postReorder(t, s, map[string][]string{agentjob.ClassBuild: {"b1", "b1"}}); w.Code != http.StatusConflict {
		t.Fatalf("duplicate job: status %d", w.Code)
	}
	// A non-ready job in the request.
	if w, _ := postReorder(t, s, map[string][]string{agentjob.ClassBuild: {"b1", "b2", "done1"}}); w.Code != http.StatusConflict {
		t.Fatalf("non-ready job: status %d", w.Code)
	}
	// Bad body.
	w := httptest.NewRecorder()
	s.handleQueueReorder(w, httptest.NewRequest("POST", "/api/queue/reorder", strings.NewReader("not json")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: status %d", w.Code)
	}
}

// TestQueueRoutesEndToEnd drives the real ServeMux (as Serve builds it) to
// confirm the queue routes are registered and wired, not just that the
// handlers behave when called directly.
func TestQueueRoutesEndToEnd(t *testing.T) {
	s := newTestServer(t)
	ingestAgentJob(t, "f1", agentjob.ClassFix, 1, nil)
	ingestAgentJob(t, "b1", agentjob.ClassBuild, 5, nil)
	ingestAgentJob(t, "g1", agentjob.ClassBackground, 1, nil)
	h := s.routes()

	// GET /api/queue: three bands in the default band order, one ready job each.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/queue", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/queue: %d %s", w.Code, w.Body.String())
	}
	var r queueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Bands) != 3 ||
		r.Bands[0].Name != agentjob.ClassFix ||
		r.Bands[1].Name != agentjob.ClassBuild ||
		r.Bands[2].Name != agentjob.ClassBackground {
		t.Fatalf("bands out of the default band order: %+v", r.Bands)
	}

	// POST /api/queue/reorder: promote background job g1 to the top of build.
	body, _ := json.Marshal(map[string]any{"orders": map[string][]string{
		agentjob.ClassFix:        {"f1"},
		agentjob.ClassBuild:      {"g1", "b1"},
		agentjob.ClassBackground: {},
	}})
	w = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/queue/reorder", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/queue/reorder: %d %s", w.Code, w.Body.String())
	}
	var r2 queueResponse
	json.Unmarshal(w.Body.Bytes(), &r2)
	if got := bandIDs(r2, agentjob.ClassBuild); strings.Join(got, ",") != "g1,b1" {
		t.Fatalf("build band = %v, want [g1 b1]", got)
	}

	// GET / still serves the static shell, with the interactive queue section.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="queue-content"`) {
		t.Fatalf("GET /: status %d, queue section missing", w.Code)
	}
}

func TestQueueBandOrderDynamic(t *testing.T) {
	s := newTestServer(t)
	ingestAgentJob(t, "f1", agentjob.ClassFix, 1, nil)
	ingestAgentJob(t, "b1", agentjob.ClassBuild, 5, nil)
	ingestAgentJob(t, "g1", agentjob.ClassBackground, 1, nil)

	// Default order is reported and the bands follow it.
	r := getQueue(t, s)
	if strings.Join(r.Order, ",") != "fix,build,background" {
		t.Fatalf("order = %v, want default fix,build,background", r.Order)
	}
	if r.Bands[0].Name != agentjob.ClassFix || r.Bands[1].Name != agentjob.ClassBuild ||
		r.Bands[2].Name != agentjob.ClassBackground {
		t.Fatalf("bands do not follow order: %+v", r.Bands)
	}

	// Re-prioritize at runtime: background runs first now.
	w, r := postBandOrder(t, s, []string{agentjob.ClassBackground, agentjob.ClassBuild, agentjob.ClassFix})
	if w.Code != http.StatusOK {
		t.Fatalf("band-order: %d %s", w.Code, w.Body.String())
	}
	if strings.Join(r.Order, ",") != "background,build,fix" {
		t.Fatalf("order after set = %v, want background,build,fix", r.Order)
	}
	if r.Bands[0].Name != agentjob.ClassBackground || r.Bands[1].Name != agentjob.ClassBuild ||
		r.Bands[2].Name != agentjob.ClassFix {
		t.Fatalf("bands not re-sorted: %+v", r.Bands)
	}

	// Drag-and-drop reorder still works under the dynamic order: move fix job
	// f1 into the top of build. Bands stay in the dynamic order in the reply.
	w, r = postReorder(t, s, map[string][]string{
		agentjob.ClassBuild:      {"f1", "b1"},
		agentjob.ClassFix:        {},
		agentjob.ClassBackground: {"g1"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("reorder: %d %s", w.Code, w.Body.String())
	}
	if strings.Join(r.Order, ",") != "background,build,fix" {
		t.Fatalf("order after reorder = %v, want background,build,fix", r.Order)
	}
	if got := bandIDs(r, agentjob.ClassBuild); strings.Join(got, ",") != "f1,b1" {
		t.Fatalf("build band = %v, want [f1 b1]", got)
	}
	st, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	if f1, err := st.Get("f1"); err != nil || f1.Class != agentjob.ClassBuild {
		t.Fatalf("f1 class = %q, %v; want build", f1.Class, err)
	}

	// The worker run order (Store.Next) follows both the new band order and
	// the reclassification: background first, then the build band.
	var got []string
	for {
		j, err := st.Next()
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			break
		}
		got = append(got, j.ID)
		if err := st.SetStatus(j.ID, agentjob.State{Status: agentjob.Skipped}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(got, ",") != "g1,f1,b1" {
		t.Fatalf("run order %v, want [g1 f1 b1]", got)
	}
}

func TestQueueBandOrderValidation(t *testing.T) {
	s := newTestServer(t)
	ingestAgentJob(t, "b1", agentjob.ClassBuild, 1, nil)

	bad := [][]string{
		nil,
		{agentjob.ClassFix},
		{agentjob.ClassFix, agentjob.ClassBuild, agentjob.ClassFix},
		{agentjob.ClassFix, agentjob.ClassBuild, "bogus"},
		{agentjob.ClassFix, agentjob.ClassBuild, agentjob.ClassBackground, agentjob.ClassFix},
	}
	for _, o := range bad {
		if w, _ := postBandOrder(t, s, o); w.Code != http.StatusBadRequest {
			t.Errorf("SetBandOrder %v: status %d, want %d", o, w.Code, http.StatusBadRequest)
		}
	}
	// The stored order is untouched by the bad requests.
	if r := getQueue(t, s); strings.Join(r.Order, ",") != "fix,build,background" {
		t.Fatalf("order after bad sets = %v, want default", r.Order)
	}
	// Bad body.
	w := httptest.NewRecorder()
	s.handleQueueBandOrder(w, httptest.NewRequest("POST", "/api/queue/band-order", strings.NewReader("not json")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: status %d", w.Code)
	}
}
