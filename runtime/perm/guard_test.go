package perm

import (
	"os"
	"path/filepath"
	"testing"
)

func guardFor(workdir string) *Guard { return &Guard{WorkDir: workdir} }

func TestCheckCommandBlocks(t *testing.T) {
	g := guardFor("/work")
	blocked := []string{
		"reboot",
		"sudo reboot now",
		"shutdown -h now",
		"agent-reboot 'test'",
		"rm -rf /etc",
		"rm -fr /home/agent",
		"rm -r /opt/jarvis",
		"sudo rm -rf /var/cache",
		"mkfs.ext4 /dev/sda1",
		"dd if=/dev/zero of=/dev/sda",
		"echo hi > /dev/sda",
		":(){ :|:& };:",
		"git push origin main",
		"git push origin master",
		"git push origin HEAD:main",
	}
	for _, c := range blocked {
		if _, b := g.CheckCommand(c); !b {
			t.Errorf("want blocked: %q", c)
		}
	}
}

func TestCheckCommandAllows(t *testing.T) {
	g := guardFor("/work")
	allowed := []string{
		"go build ./...",
		"rm -rf ./tmp/scratch",
		"rm -rf /work/sub/dir",
		"git push origin feature-xyz",
		"git push -u origin phase-1-superagent",
		"ls -la /work",
		"cat /etc/hostname", // reading a system file is fine
		"echo 'rm -rf /etc' > notes.txt", // mentioning in text is not running it…
	}
	for _, c := range allowed {
		if reason, b := g.CheckCommand(c); b {
			t.Errorf("want allowed %q, got blocked: %s", c, reason)
		}
	}
}

func TestCheckCommandDisabled(t *testing.T) {
	g := guardFor("/work")
	g.Disabled = true
	if _, b := g.CheckCommand("reboot"); b {
		t.Fatal("disabled guard must not block")
	}
}

func TestCheckPath(t *testing.T) {
	g := guardFor("/work")
	if abs, _, b := g.CheckPath("/work/a/b.txt"); b || abs != "/work/a/b.txt" {
		t.Fatalf("got %q blocked=%v", abs, b)
	}
	if abs, _, b := g.CheckPath("relative/x.txt"); b {
		t.Fatalf("relative path escaped: %q", abs)
	}
	if _, reason, b := g.CheckPath("/etc/passwd"); !b {
		t.Fatalf("want /etc/passwd blocked, got %q", reason)
	}
	if _, _, b := g.CheckPath("/work/../etc/passwd"); !b {
		t.Fatal("want /work/../etc/passwd blocked")
	}
	// Extra dirs are allowed.
	g2 := &Guard{WorkDir: "/work", ExtraDirs: []string{"/scratch"}}
	if _, _, b := g2.CheckPath("/scratch/data.txt"); b {
		t.Fatal("extra dir should be allowed")
	}
}

func TestCheckPathSymlinkEscape(t *testing.T) {
	work, err := os.MkdirTemp("", "perm-work")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)
	outside, err := os.MkdirTemp("", "perm-outside")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(outside)
	link := filepath.Join(work, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	g := guardFor(work)
	// A path through the symlink resolves outside the work dir: refused.
	if _, reason, b := g.CheckPath(filepath.Join(link, "secret.txt")); !b {
		t.Fatalf("want path through symlink blocked, got %q", reason)
	}
	// The symlink itself (an existing link pointing outside) is refused.
	if _, _, b := g.CheckPath(link); !b {
		t.Fatal("want the symlink itself blocked")
	}
	// A real path inside the work dir is still allowed, returned resolved.
	if abs, _, b := g.CheckPath(filepath.Join(work, "ok.txt")); b || abs != filepath.Join(work, "ok.txt") {
		t.Fatalf("path inside the work dir must be allowed, got %q blocked=%v", abs, b)
	}
}

func TestRecursiveRmOutside(t *testing.T) {
	root := filepath.Join("/work", "a")
	if _, ok := recursiveRmOutside("rm -rf "+root, "/work"); ok {
		t.Fatal("rm inside the work dir must be allowed")
	}
	if target, ok := recursiveRmOutside("rm -rf /etc/apt", "/work"); !ok || target != "/etc/apt" {
		t.Fatalf("got %q, %v", target, ok)
	}
	// Non-recursive rm of an absolute path is allowed (no r flag).
	if _, ok := recursiveRmOutside("rm /etc/some-file", "/work"); ok {
		t.Fatal("plain rm of an absolute path is allowed")
	}
}
