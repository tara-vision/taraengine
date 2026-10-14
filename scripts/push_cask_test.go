package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs git in dir and returns its output, failing the test on an error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newTap is a bare repository shaped like the tap: one commit with taracode's cask in it.
func newTap(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "tap.git")
	git(t, root, "init", "--quiet", "--bare", "--initial-branch=main", bare)
	work := filepath.Join(root, "work")
	git(t, root, "clone", "--quiet", bare, work)
	if err := os.MkdirAll(filepath.Join(work, "Casks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "Casks", "taracode.rb"), []byte("cask \"taracode\" do\nend\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", "Update taracode cask to v3.2.6")
	git(t, work, "push", "--quiet", "origin", "HEAD:main")
	return bare
}

// pushCask runs scripts/push-cask.sh against the tap with the cask body and the tag.
func pushCask(t *testing.T, tap, body, tag string) string {
	t.Helper()
	cask := filepath.Join(t.TempDir(), "taraengine.rb")
	if err := os.WriteFile(cask, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "push-cask.sh", cask, tag)
	cmd.Env = append(os.Environ(), "TAP_URL="+tap, "TAP_TOKEN=", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("push-cask: %v\n%s", err, out)
	}
	return string(out)
}

func TestPushCaskCommitsTheCaskToTheTapOnce(t *testing.T) {
	tap := newTap(t)
	out := pushCask(t, tap, "cask \"taraengine\" do\n  version \"0.3.0\"\nend\n", "v0.3.0")
	if !strings.Contains(out, "pushed the taraengine cask for v0.3.0") {
		t.Fatalf("%s", out)
	}
	if got := git(t, tap, "log", "-1", "--format=%s%n%an"); got != "Update taraengine cask to v0.3.0\ngithub-actions[bot]" {
		t.Fatalf("the commit: %q", got)
	}
	if got := git(t, tap, "show", "HEAD:Casks/taraengine.rb"); !strings.Contains(got, `version "0.3.0"`) {
		t.Fatalf("the cask: %q", got)
	}
	if got := git(t, tap, "show", "HEAD:Casks/taracode.rb"); got != "cask \"taracode\" do\nend" {
		t.Fatalf("taracode's cask is left alone: %q", got)
	}
	out = pushCask(t, tap, "cask \"taraengine\" do\n  version \"0.3.0\"\nend\n", "v0.3.0")
	if !strings.Contains(out, "already at v0.3.0") || git(t, tap, "rev-list", "--count", "HEAD") != "2" {
		t.Fatalf("a re-run of the job changes nothing: %s", out)
	}
	pushCask(t, tap, "cask \"taraengine\" do\n  version \"0.3.1\"\nend\n", "v0.3.1")
	if got := git(t, tap, "log", "-1", "--format=%s"); got != "Update taraengine cask to v0.3.1" {
		t.Fatalf("the next release: %q", got)
	}
}
