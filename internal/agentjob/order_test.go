package agentjob

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newOrderStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func ingestClass(t *testing.T, st *Store, id, class string, priority int) {
	t.Helper()
	if _, err := st.Ingest(Spec{ID: id, Title: id, Class: class, Priority: priority}); err != nil {
		t.Fatal(err)
	}
}

// nextIDs runs the store's Next loop and returns the run order.
func nextIDs(t *testing.T, st *Store) []string {
	t.Helper()
	var got []string
	for {
		j, err := st.Next()
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			return got
		}
		got = append(got, j.ID)
		if err := st.SetStatus(j.ID, State{Status: Skipped}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBandOrderDefaults(t *testing.T) {
	st := newOrderStore(t)
	order, err := st.BandOrder()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "fix,build,background" {
		t.Fatalf("BandOrder = %v, want default fix,build,background", order)
	}
}

func TestSetBandOrderPersistsAndDrivesRunOrder(t *testing.T) {
	st := newOrderStore(t)
	ingestClass(t, st, "f1", ClassFix, 1)
	ingestClass(t, st, "b1", ClassBuild, 9)
	ingestClass(t, st, "g1", ClassBackground, 1)

	// Default order: the fix job runs first.
	if got := nextIDs(t, st); strings.Join(got, ",") != "f1,b1,g1" {
		t.Fatalf("default run order %v, want [f1 b1 g1]", got)
	}

	if err := st.SetBandOrder([]string{ClassBackground, ClassBuild, ClassFix}); err != nil {
		t.Fatal(err)
	}
	// The order survives reopening the store.
	st2, err := Open(st.dir)
	if err != nil {
		t.Fatal(err)
	}
	order, err := st2.BandOrder()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "background,build,fix" {
		t.Fatalf("BandOrder after set = %v, want background,build,fix", order)
	}

	// The new order is reflected in the run order (f1/b1/g1 are done).
	ingestClass(t, st2, "f2", ClassFix, 5)
	ingestClass(t, st2, "b2", ClassBuild, 2)
	ingestClass(t, st2, "g2", ClassBackground, 3)
	if got := nextIDs(t, st2); strings.Join(got, ",") != "g2,b2,f2" {
		t.Fatalf("run order after set %v, want [g2 b2 f2]", got)
	}
}

func TestSetBandOrderValidates(t *testing.T) {
	st := newOrderStore(t)
	bad := [][]string{
		{},
		{ClassFix},
		{ClassFix, ClassBuild, ClassFix},
		{ClassFix, ClassBuild, "bogus"},
		{ClassFix, ClassBuild, ClassBackground, ClassFix},
	}
	for _, o := range bad {
		if err := st.SetBandOrder(o); err == nil {
			t.Errorf("SetBandOrder(%v) = nil, want error", o)
		}
	}
	order, err := st.BandOrder()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "fix,build,background" {
		t.Fatalf("BandOrder after bad sets = %v, want default", order)
	}
	if err := st.SetBandOrder([]string{ClassBuild, ClassFix, ClassBackground}); err != nil {
		t.Fatalf("SetBandOrder(valid permutation) = %v", err)
	}
}

func TestBandOrderCorruptFileFallsBack(t *testing.T) {
	st := newOrderStore(t)
	for _, content := range []string{"not json", `["fix","fix"]`, `["fix","build","build","background"]`} {
		if err := os.WriteFile(filepath.Join(st.dir, bandOrderFile), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		order, err := st.BandOrder()
		if err != nil {
			t.Fatalf("BandOrder with %q: %v", content, err)
		}
		if strings.Join(order, ",") != "fix,build,background" {
			t.Fatalf("BandOrder with %q = %v, want default fallback", content, order)
		}
	}
}

func TestRankFollowsRuntimeOrder(t *testing.T) {
	st := newOrderStore(t)
	if r, err := st.Rank(ClassBuild, ""); err != nil || r != 1 {
		t.Fatalf("Rank(build) = %d, %v; want 1, nil", r, err)
	}
	if err := st.SetBandOrder([]string{ClassBuild, ClassFix, ClassBackground}); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{ClassBuild: 0, ClassFix: 1, ClassBackground: 2}
	for c, w := range want {
		if r, err := st.Rank(c, ""); err != nil || r != w {
			t.Fatalf("Rank(%s) = %d, %v; want %d", c, r, err, w)
		}
	}
}
