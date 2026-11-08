package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/photo"
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
	if len(r.Bands) != 4 ||
		r.Bands[0].Name != PhotoBand ||
		r.Bands[1].Name != agentjob.ClassFix ||
		r.Bands[2].Name != agentjob.ClassBuild ||
		r.Bands[3].Name != agentjob.ClassBackground {
		t.Fatalf("bands out of order (photo band first, then the runtime band order): %+v", r.Bands)
	}
	if len(r.Bands[0].Jobs) != 0 {
		t.Fatalf("photo band = %+v, want empty", r.Bands[0].Jobs)
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

	// GET /api/queue: the photo band plus the three class bands in the
	// default band order, one ready job in each class band.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/queue", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/queue: %d %s", w.Code, w.Body.String())
	}
	var r queueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Bands) != 4 ||
		r.Bands[0].Name != PhotoBand ||
		r.Bands[1].Name != agentjob.ClassFix ||
		r.Bands[2].Name != agentjob.ClassBuild ||
		r.Bands[3].Name != agentjob.ClassBackground {
		t.Fatalf("bands out of order: %+v", r.Bands)
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

	// Default order is reported and the bands follow it (photo band first,
	// outside the runtime band order).
	r := getQueue(t, s)
	if strings.Join(r.Order, ",") != "fix,build,background" {
		t.Fatalf("order = %v, want default fix,build,background", r.Order)
	}
	if r.Bands[0].Name != PhotoBand || r.Bands[1].Name != agentjob.ClassFix ||
		r.Bands[2].Name != agentjob.ClassBuild || r.Bands[3].Name != agentjob.ClassBackground {
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
	if r.Bands[0].Name != PhotoBand || r.Bands[1].Name != agentjob.ClassBackground ||
		r.Bands[2].Name != agentjob.ClassBuild || r.Bands[3].Name != agentjob.ClassFix {
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

// ingestPhotoJob adds a ready photo-tagged job plus its prompt sidecar — the
// same pair of files jarvis-chat's !photo enqueue writes.
func ingestPhotoJob(t *testing.T, id, class string, priority int, prompt string) {
	t.Helper()
	st, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := agentjob.Spec{
		ID:           id,
		Title:        "photo: " + id,
		Instructions: "run jarvis-photo make --job " + id,
		Class:        class,
		Priority:     priority,
		Tags:         []string{photo.Tag},
	}
	if _, err := st.Ingest(spec); err != nil {
		t.Fatal(err)
	}
	if err := photo.WritePrompt(st.Dir(), id, prompt); err != nil {
		t.Fatal(err)
	}
}

func postEdit(t *testing.T, s *Server, id, title, instructions string) (*httptest.ResponseRecorder, queueResponse) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"id": id, "title": title, "instructions": instructions})
	req := httptest.NewRequest("POST", "/api/queue/edit", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleQueueEdit(w, req)
	var r queueResponse
	if w.Code == http.StatusOK {
		json.Unmarshal(w.Body.Bytes(), &r)
	}
	return w, r
}

// TestQueuePhotoBand covers the dedicated photo band: photo-tagged ready
// jobs are listed first in their own display-only band (with the sidecar
// prompt as their editable text) and are absent from the class bands; the
// runtime band order is unaffected.
func TestQueuePhotoBand(t *testing.T) {
	s := newTestServer(t)
	ingestPhotoJob(t, "p1", agentjob.ClassBuild, 999, "a red fox in a snowstorm")
	ingestPhotoJob(t, "p2", agentjob.ClassFix, 1000, "a lighthouse at dawn")
	ingestAgentJob(t, "b1", agentjob.ClassBuild, 5, nil)
	ingestAgentJob(t, "pd", agentjob.ClassBuild, 50, nil)
	setAgentStatus(t, "pd", agentjob.Done) // a done photo job is not queued

	r := getQueue(t, s)
	if len(r.Bands) != 4 || r.Bands[0].Name != PhotoBand {
		t.Fatalf("photo band missing or out of place: %+v", r.Bands)
	}
	if got := bandIDs(r, PhotoBand); strings.Join(got, ",") != "p2,p1" {
		t.Fatalf("photo band = %v, want [p2 p1] (priority order within the batch)", got)
	}
	if got := bandIDs(r, agentjob.ClassBuild); strings.Join(got, ",") != "b1" {
		t.Fatalf("build band = %v, want only [b1] (photo jobs split out)", got)
	}
	if len(r.Order) != 3 || strings.Join(r.Order, ",") != "fix,build,background" {
		t.Fatalf("order = %v, want the 3-class runtime band order unchanged", r.Order)
	}
	// The photo job carries its sidecar prompt as the editable text and keeps
	// its class for the run order.
	var p1 *queueJob
	for i := range r.Bands[0].Jobs {
		if r.Bands[0].Jobs[i].ID == "p1" {
			p1 = &r.Bands[0].Jobs[i]
		}
	}
	if p1 == nil || !p1.Photo {
		t.Fatalf("p1 not flagged photo in the photo band: %+v", r.Bands[0].Jobs)
	}
	if p1.Prompt != "a red fox in a snowstorm" {
		t.Errorf("p1 prompt = %q, want the sidecar prompt", p1.Prompt)
	}
	if p1.Class != agentjob.ClassBuild {
		t.Errorf("p1 class = %q, want build (run order is unchanged)", p1.Class)
	}
}

// TestQueuePhotoInFlight covers in-flight photo jobs: a claimed/running
// photo-tagged job appears at the head of the photo band (marked in_flight,
// with the round's started time) ahead of the ready batch, and stays out of
// the class bands, the reorder, and the edit. A running non-photo job is not
// in the queue at all (the Agent Jobs panel covers it).
func TestQueuePhotoInFlight(t *testing.T) {
	s := newTestServer(t)
	ingestPhotoJob(t, "p-run", agentjob.ClassBuild, 999, "a red fox in a snowstorm")
	ingestPhotoJob(t, "p-wait", agentjob.ClassFix, 1000, "a lighthouse at dawn")
	ingestAgentJob(t, "b1", agentjob.ClassBuild, 5, nil)
	ingestAgentJob(t, "n1", agentjob.ClassBuild, 9, nil)
	st, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus("p-run", agentjob.State{Status: agentjob.Running, RanWith: "igpu", StartedAt: "2026-11-06T03:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus("n1", agentjob.State{Status: agentjob.Claimed, RanWith: "local", StartedAt: "2026-11-06T03:00:01Z"}); err != nil {
		t.Fatal(err)
	}

	r := getQueue(t, s)
	// The in-flight photo job leads the photo band, ahead of the ready batch.
	if got := bandIDs(r, PhotoBand); strings.Join(got, ",") != "p-run,p-wait" {
		t.Fatalf("photo band = %v, want [p-run p-wait] (in flight first)", got)
	}
	var prun *queueJob
	for i := range r.Bands[0].Jobs {
		if r.Bands[0].Jobs[i].ID == "p-run" {
			prun = &r.Bands[0].Jobs[i]
		}
	}
	if prun == nil || !prun.InFlight || !prun.Photo {
		t.Fatalf("p-run not flagged in flight: %+v", r.Bands[0].Jobs)
	}
	if prun.StartedAt != "2026-11-06 03:00" {
		t.Errorf("p-run started_at = %q, want 2026-11-06 03:00", prun.StartedAt)
	}
	if prun.Prompt != "a red fox in a snowstorm" {
		t.Errorf("p-run prompt = %q, want the sidecar prompt", prun.Prompt)
	}
	// The running non-photo job is not in the queue at all.
	for _, b := range r.Bands {
		for _, j := range b.Jobs {
			if j.ID == "n1" {
				t.Fatalf("running non-photo job n1 shown in band %q: %+v", b.Name, b.Jobs)
			}
		}
	}
	// The ready build job is unaffected.
	if got := bandIDs(r, agentjob.ClassBuild); strings.Join(got, ",") != "b1" {
		t.Fatalf("build band = %v, want [b1]", got)
	}

	// Reorder refuses the in-flight job; omitting it (as the UI does) is fine.
	if w, _ := postReorder(t, s, map[string][]string{
		agentjob.ClassBuild: {"b1", "p-run"},
		agentjob.ClassFix:   {"p-wait"},
	}); w.Code != http.StatusConflict {
		t.Fatalf("reorder listing in-flight p-run: status %d, want %d", w.Code, http.StatusConflict)
	}
	w, r := postReorder(t, s, map[string][]string{
		agentjob.ClassBuild: {"b1"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("reorder omitting in-flight photo job: %d %s", w.Code, w.Body.String())
	}
	if got := bandIDs(r, PhotoBand); strings.Join(got, ",") != "p-run,p-wait" {
		t.Fatalf("photo band after reorder = %v, want [p-run p-wait]", got)
	}
	// Edit refuses the in-flight job.
	if w, _ := postEdit(t, s, "p-run", "x", "y"); w.Code != http.StatusConflict {
		t.Fatalf("edit in-flight p-run: status %d, want %d", w.Code, http.StatusConflict)
	}
}

// TestQueueReorderWithPhotoJobs covers the reorder contract with photo jobs
// present: they are exempt from the exactly-once rule (the client may omit
// them), and a photo job listed under a class band is reclassified like any
// other job.
func TestQueueReorderWithPhotoJobs(t *testing.T) {
	s := newTestServer(t)
	ingestPhotoJob(t, "p1", agentjob.ClassBuild, 999, "a red fox")
	ingestAgentJob(t, "b1", agentjob.ClassBuild, 1, nil)
	ingestAgentJob(t, "b2", agentjob.ClassBuild, 9, nil)

	// The photo job is omitted (the UI never posts it): accepted, and it
	// stays in the photo band.
	w, r := postReorder(t, s, map[string][]string{
		agentjob.ClassBuild: {"b1", "b2"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("reorder omitting the photo job: %d %s", w.Code, w.Body.String())
	}
	if got := bandIDs(r, PhotoBand); strings.Join(got, ",") != "p1" {
		t.Fatalf("photo band = %v, want [p1]", got)
	}
	if got := bandIDs(r, agentjob.ClassBuild); strings.Join(got, ",") != "b1,b2" {
		t.Fatalf("build band = %v, want [b1 b2]", got)
	}

	// The photo job listed under a class band reclassifies (old-frontend
	// behavior) and still shows in the photo band — membership is by tag.
	// The other ready jobs are still listed, as always.
	w, r = postReorder(t, s, map[string][]string{
		agentjob.ClassFix:   {"p1"},
		agentjob.ClassBuild: {"b1", "b2"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("reorder listing the photo job under fix: %d %s", w.Code, w.Body.String())
	}
	if got := bandIDs(r, PhotoBand); strings.Join(got, ",") != "p1" {
		t.Fatalf("photo band = %v, want [p1]", got)
	}
	st, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	if j, err := st.Get("p1"); err != nil || j.Class != agentjob.ClassFix {
		t.Fatalf("p1 class = %q, %v; want fix after reclassification", j.Class, err)
	}
}

// TestQueueEdit covers editing the text of a queued job: ordinary jobs get
// their title/instructions rewritten in job.json; a photo job's text is the
// generation prompt, so it goes to the sidecar file and the generic
// instructions in job.json are left as-is; not-ready, unknown, and empty
// edits are refused.
func TestQueueEdit(t *testing.T) {
	s := newTestServer(t)
	ingestAgentJob(t, "b1", agentjob.ClassBuild, 5, nil)
	ingestPhotoJob(t, "p1", agentjob.ClassBuild, 999, "a red fox in a snowstorm")
	ingestAgentJob(t, "d1", agentjob.ClassBuild, 1, nil)
	setAgentStatus(t, "d1", agentjob.Done)

	// Ordinary job: title and instructions are replaced.
	w, r := postEdit(t, s, "b1", "b1 edited", "do the thing, but carefully")
	if w.Code != http.StatusOK {
		t.Fatalf("edit b1: %d %s", w.Code, w.Body.String())
	}
	st, err := agentjob.Open(agentjob.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	j, err := st.Get("b1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Title != "b1 edited" || j.Instructions != "do the thing, but carefully" {
		t.Fatalf("b1 = %+v, want edited title and instructions", j)
	}
	if got := bandIDs(r, agentjob.ClassBuild); len(got) != 1 || got[0] != "b1" {
		t.Fatalf("build band after edit = %v, want [b1]", got)
	}
	var b1 *queueJob
	for i := range r.Bands {
		for k := range r.Bands[i].Jobs {
			if r.Bands[i].Jobs[k].ID == "b1" {
				b1 = &r.Bands[i].Jobs[k]
			}
		}
	}
	if b1 == nil || b1.Title != "b1 edited" || b1.Instructions != "do the thing, but carefully" {
		t.Fatalf("edited job not reflected in the queue response: %+v", b1)
	}

	// Photo job: the text goes to the prompt sidecar; job.json keeps its
	// generic instructions (the iGPU round acts on them, not the prompt).
	w, _ = postEdit(t, s, "p1", "p1 edited", "a blue whale, cinematic")
	if w.Code != http.StatusOK {
		t.Fatalf("edit p1: %d %s", w.Code, w.Body.String())
	}
	pb, err := os.ReadFile(photo.PromptPath(st.Dir(), "p1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(pb) != "a blue whale, cinematic" {
		t.Errorf("sidecar = %q, want the new prompt", pb)
	}
	j, err = st.Get("p1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Title != "p1 edited" {
		t.Errorf("p1 title = %q, want the edited title", j.Title)
	}
	if j.Instructions != "run jarvis-photo make --job p1" {
		t.Errorf("p1 instructions = %q, want the generic ones unchanged", j.Instructions)
	}
	// The next queue read offers the new prompt as p1's editable text.
	r = getQueue(t, s)
	var p1 *queueJob
	for i := range r.Bands {
		for k := range r.Bands[i].Jobs {
			if r.Bands[i].Jobs[k].ID == "p1" {
				p1 = &r.Bands[i].Jobs[k]
			}
		}
	}
	if p1 == nil || !p1.Photo || p1.Prompt != "a blue whale, cinematic" {
		t.Fatalf("p1 after edit = %+v, want photo job with the new prompt", p1)
	}

	// A not-ready job is refused.
	if w, _ := postEdit(t, s, "d1", "x", "y"); w.Code != http.StatusConflict {
		t.Fatalf("edit done job: status %d, want %d", w.Code, http.StatusConflict)
	}
	// An unknown job is a 404.
	if w, _ := postEdit(t, s, "nope", "x", "y"); w.Code != http.StatusNotFound {
		t.Fatalf("edit unknown job: status %d, want %d", w.Code, http.StatusNotFound)
	}
	// Nothing to edit is a 400.
	if w, _ := postEdit(t, s, "b1", "", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("empty edit: status %d, want %d", w.Code, http.StatusBadRequest)
	}
	// Bad body is a 400.
	req := httptest.NewRequest("POST", "/api/queue/edit", strings.NewReader("not json"))
	w = httptest.NewRecorder()
	s.handleQueueEdit(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: status %d", w.Code)
	}
	// A bad id is a 400.
	if w, _ := postEdit(t, s, "../etc", "x", "y"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id: status %d, want %d", w.Code, http.StatusBadRequest)
	}
}
