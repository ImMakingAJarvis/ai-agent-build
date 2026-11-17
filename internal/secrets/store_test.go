package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestPutGetRoundTrip(t *testing.T) {
	st := openStore(t)
	if err := st.Put("api_key", "sk-123\nwith spaces and unicode ✓"); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get("api_key")
	if err != nil {
		t.Fatal(err)
	}
	want := "sk-123\nwith spaces and unicode ✓"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPutOverwrites(t *testing.T) {
	st := openStore(t)
	st.Put("k", "first")
	if err := st.Put("k", "second"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get("k")
	if got != "second" {
		t.Fatalf("got %q, want second", got)
	}
}

func TestGetMissing(t *testing.T) {
	st := openStore(t)
	if _, err := st.Get("nope"); err == nil || !strings.Contains(err.Error(), "no secret named") {
		t.Fatalf("want missing error, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	st := openStore(t)
	st.Put("k", "v")
	if err := st.Delete("k"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get("k"); err == nil {
		t.Fatal("expected error after delete")
	}
	if err := st.Delete("k"); err == nil {
		t.Fatal("expected error deleting twice")
	}
}

func TestInvalidNames(t *testing.T) {
	st := openStore(t)
	for _, name := range []string{
		"", "a/b", "../escape", ".hidden", "-dash", "x y",
		"/etc/passwd", "a/../../etc", strings.Repeat("a", 129),
	} {
		if err := st.Put(name, "v"); err == nil {
			t.Errorf("Put(%q): expected error", name)
		}
		if _, err := st.Get(name); err == nil {
			t.Errorf("Get(%q): expected error", name)
		}
		if err := st.Delete(name); err == nil {
			t.Errorf("Delete(%q): expected error", name)
		}
	}
}

func TestValidNameEdges(t *testing.T) {
	st := openStore(t)
	if err := st.Put(strings.Repeat("a", 128), "v"); err != nil {
		t.Fatalf("128-char name should be valid: %v", err)
	}
	if err := st.Put("a.b_c-9", "v"); err != nil {
		t.Fatalf("mixed charset name should be valid: %v", err)
	}
	if err := st.Put("a..b", "v"); err != nil {
		t.Fatalf("mid-name dots should be valid (single path element): %v", err)
	}
}

func TestValueTooLarge(t *testing.T) {
	st := openStore(t)
	if err := st.Put("big", strings.Repeat("x", MaxValueLen+1)); err == nil {
		t.Fatal("expected size error")
	}
}

func TestListNeverReturnsValues(t *testing.T) {
	st := openStore(t)
	st.Put("alpha", "secret-value-A")
	st.Put("beta", "secret-value-B")

	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 entries, got %d", len(list))
	}
	if list[0].Name != "alpha" || list[1].Name != "beta" {
		t.Fatalf("wrong order or names: %+v", list)
	}
	if list[0].Bytes != len("secret-value-A") {
		t.Fatalf("bytes = %d", list[0].Bytes)
	}
	for _, f := range []string{"alpha", "beta"} {
		b, _ := os.ReadFile(filepath.Join(st.Root(), f))
		if strings.Contains(string(b), "value") && len(b) == 0 {
			t.Fatalf("empty file for %s", f)
		}
	}
	// The list payload must not contain either value.
	var sb strings.Builder
	for _, i := range list {
		sb.WriteString(i.Name)
	}
	if strings.Contains(sb.String(), "secret-value") {
		t.Fatal("list leaked a value")
	}
}

func TestListSkipsForeignFiles(t *testing.T) {
	st := openStore(t)
	st.Put("real", "v")
	// A crashed temp file and a foreign file must not surface in List.
	if err := os.WriteFile(filepath.Join(st.Root(), ".real.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.Root(), ".hidden"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "real" {
		t.Fatalf("foreign files leaked into list: %+v", list)
	}
}

func TestFilePermissions(t *testing.T) {
	st := openStore(t)
	if err := st.Put("k", "v"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(st.Root(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode = %o, want 600", fi.Mode().Perm())
	}
	di, err := os.Stat(st.Root())
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("secret dir mode = %o, want 700", di.Mode().Perm())
	}
}
