// redact_test.go covers card ELR-91: known values are replaced by
// "[redacted]", every token pattern is, a private key block goes whole,
// and FromEnv/FromDir collect the known values. Every secret in here is
// made up.
package redact

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestKnownValue: a known secret value is replaced wherever it appears,
// and when one value is a prefix of another the longest is replaced
// whole (longest first).
func TestKnownValue(t *testing.T) {
	r := New([]string{"mysecretpassword", "abcd1234", "abcd12345"})
	if got, want := r.Apply("the password is mysecretpassword, ok"),
		"the password is [redacted], ok"; got != want {
		t.Errorf("Apply = %q, want %q", got, want)
	}
	if got, want := r.Apply("x abcd12345 y"), "x [redacted] y"; got != want {
		t.Errorf("prefix value: Apply = %q, want %q (longest first)", got, want)
	}
	if got, want := r.Apply("abcd1234 and abcd12345"),
		"[redacted] and [redacted]"; got != want {
		t.Errorf("Apply = %q, want %q", got, want)
	}
}

// TestShortValueIgnored: a value shorter than 8 characters is ignored —
// it would redact ordinary words — but the patterns still apply.
func TestShortValueIgnored(t *testing.T) {
	r := New([]string{"short7", "12345678"})
	if got, want := r.Apply("short7 and 12345678"), "short7 and [redacted]"; got != want {
		t.Errorf("Apply = %q, want %q", got, want)
	}
	r2 := New([]string{"abc"})
	if got, want := r2.Apply("abc plus ghp_"+strings.Repeat("b", 36)),
		"abc plus [redacted]"; got != want {
		t.Errorf("Apply with only short values = %q, want %q", got, want)
	}
}

// TestPatterns: one row per token pattern, plus ordinary text that must
// not change.
func TestPatterns(t *testing.T) {
	r := New(nil)
	rows := []struct{ in, want string }{
		{"token ghp_" + strings.Repeat("a", 36), "token [redacted]"},
		{"pat github_pat_" + strings.Repeat("A", 22), "pat [redacted]"},
		{"ghs_" + strings.Repeat("c", 37), "[redacted]"},
		{"sk-ant-" + strings.Repeat("d", 25), "[redacted]"},
		{"plane_api_" + strings.Repeat("0123456789abcdef", 2), "[redacted]"},
		{"aws AKIA" + strings.Repeat("B", 16), "aws [redacted]"},
		{"xoxb-" + strings.Repeat("1", 12), "[redacted]"},
		{"Authorization: Bearer " + strings.Repeat("e", 24), "Authorization: Bearer [redacted]"},
		{"Authorization: bearer " + strings.Repeat("f", 20) + "==", "Authorization: bearer [redacted]"},
		{"hello world, nothing to see here", "hello world, nothing to see here"},
	}
	for i, row := range rows {
		if got := r.Apply(row.in); got != row.want {
			t.Errorf("row %d: Apply(%q...) = %q, want %q", i, row.in[:min(len(row.in), 20)], got, row.want)
		}
	}
}

// TestPrivateKeyBlock: a whole PEM private key block is redacted, from
// the BEGIN line to the matching END line; a public key block stays.
func TestPrivateKeyBlock(t *testing.T) {
	r := New(nil)
	block := "-----BEGIN RSA PRIVATE KEY-----\n" +
		"MIIEpAIBAAKCAQEA7madeupbase64data\n" +
		"moremadeupbase64data\n" +
		"-----END RSA PRIVATE KEY-----"
	if got, want := r.Apply("header\n"+block+"\nfooter"),
		"header\n[redacted]\nfooter"; got != want {
		t.Errorf("Apply = %q, want %q", got, want)
	}
	// PKCS#8 has no algorithm name in the label.
	block2 := "-----BEGIN PRIVATE KEY-----\nMIIabc\n-----END PRIVATE KEY-----"
	if got, want := r.Apply(block2), "[redacted]"; got != want {
		t.Errorf("PKCS#8 block: Apply = %q, want %q", got, want)
	}
	// A public key block is not a private key: it stays.
	pub := "-----BEGIN PUBLIC KEY-----\nMIIabc\n-----END PUBLIC KEY-----"
	if got := r.Apply(pub); got != pub {
		t.Errorf("public key block changed: %q", got)
	}
}

// TestFromEnv: only the values of variables whose name contains TOKEN,
// KEY, SECRET, PASSWORD or PASS (case-insensitive) come back.
func TestFromEnv(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin:/bin",
		"GITHUB_TOKEN=madeuptokenvalue123456",
		"MY_API_KEY=madeupkeyvalue424242",
		"db_password=madeuppwvalue777777",
		"HOME=/home/agent",
		"NO_EQUALS_HERE",
		"PASS_CODE=madeuppasscode999999",
		"AWS_SECRET_ACCESS_KEY=madeup=value",
	}
	got := FromEnv(environ)
	want := []string{
		"madeuptokenvalue123456",
		"madeupkeyvalue424242",
		"madeuppwvalue777777",
		"madeuppasscode999999",
		"madeup=value",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FromEnv = %v, want %v", got, want)
	}
}

// TestFromDir: the trimmed content of every regular file at most 4096
// bytes comes back (in filename order); a bigger file and a directory
// do not; a missing dir returns nil.
func TestFromDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("  madeupdirvalue111222333\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	edge := strings.Repeat("y", 4095) + "\n" // exactly 4096 bytes: kept
	if err := os.WriteFile(filepath.Join(dir, "edge"), []byte(edge), 0o600); err != nil {
		t.Fatal(err)
	}
	toobig := strings.Repeat("z", 4096) + "\n" // 4097 bytes: skipped
	if err := os.WriteFile(filepath.Join(dir, "toobig"), []byte(toobig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := FromDir(dir)
	want := []string{strings.Repeat("y", 4095), "madeupdirvalue111222333"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FromDir = %d values (last %q), want %d values (token trimmed, 4096-byte file, no dir)",
			len(got), lastStr(got), len(want))
	}

	if got := FromDir(filepath.Join(dir, "missing")); got != nil {
		t.Errorf("FromDir(missing) = %v, want nil", got)
	}
}

// TestNilRedactor: a nil *Redactor returns its input unchanged.
func TestNilRedactor(t *testing.T) {
	var r *Redactor
	in := "short7 and ghp_" + strings.Repeat("a", 36) + " and a bearer " + strings.Repeat("x", 20)
	if got := r.Apply(in); got != in {
		t.Errorf("nil Redactor changed the text: %q", got)
	}
}

func lastStr(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[len(ss)-1]
}
