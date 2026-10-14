package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// golden compares got with testdata/name, and rewrites the file when UPDATE_GOLDEN is set.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s differs from what was rendered:\n%s", path, got)
	}
}

func TestThePlistRunsTheServiceAndRestartsItAfterACrash(t *testing.T) {
	got := RenderPlist("/opt/homebrew/bin/taraengine", "/Users/someone/Library/Logs/taraengine/service.log")
	golden(t, "vision.tara.taraengine.plist", got)
}

func TestThePlistEscapesWhatXMLWouldMisread(t *testing.T) {
	got := RenderPlist("/Users/a&b/<bin>/taraengine", "/Users/a&b/Library/Logs/taraengine/service.log")
	if !strings.Contains(string(got), "<string>/Users/a&amp;b/&lt;bin&gt;/taraengine</string>") ||
		!strings.Contains(string(got), "<string>/Users/a&amp;b/Library/Logs/taraengine/service.log</string>") {
		t.Fatalf("the paths are escaped:\n%s", got)
	}
}

func TestTheUnitRunsTheServiceAsItsOwnUser(t *testing.T) {
	got, err := RenderUnit("/usr/local/bin/taraengine")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "taraengine.service", got)
}

func TestTheUnitRefusesABinaryPathSystemdWouldReadOtherwise(t *testing.T) {
	for _, path := range []string{"/opt/my tools/taraengine", "/opt/50%/taraengine", `/opt/"q"/taraengine`, "bin/taraengine", ""} {
		if _, err := RenderUnit(path); err == nil || !strings.Contains(err.Error(), "binary path") {
			t.Errorf("%q: %v", path, err)
		}
	}
}

func TestTheLinuxConfigNamesTheDataDirectoryOnly(t *testing.T) {
	golden(t, "config.yaml", LinuxConfig())
}

// requireTool is the path of a tool that checks rendered files. Off CI a missing tool skips the check; on CI, where
// the job installs it, it fails.
func requireTool(t *testing.T, goos, name string) string {
	t.Helper()
	if runtime.GOOS != goos {
		t.Skipf("%s checks run on %s", name, goos)
	}
	path, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is required on CI: %v", name, err)
		}
		t.Skipf("%s is not installed: %v", name, err)
	}
	return path
}

func TestThePlistPassesPlutil(t *testing.T) {
	plutil := requireTool(t, "darwin", "plutil")
	got := RenderPlist("/opt/homebrew/bin/taraengine", "/Users/someone/Library/Logs/taraengine/service.log")
	path := filepath.Join(t.TempDir(), Label+".plist")
	if err := os.WriteFile(path, got, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil: %v\n%s", err, out)
	}
}

func TestTheUnitPassesSystemdAnalyze(t *testing.T) {
	analyze := requireTool(t, "linux", "systemd-analyze")
	binary, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	got, err := RenderUnit(binary)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), UnitName)
	if err := os.WriteFile(path, got, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(analyze, "verify", path).CombinedOutput(); err != nil {
		t.Fatalf("systemd-analyze verify: %v\n%s", err, out)
	}
}

func TestRenderReportsATemplateItDoesNotHave(t *testing.T) {
	if _, err := render("nope.tmpl", nil); err == nil || !strings.HasPrefix(err.Error(), "service: ") {
		t.Fatalf("got %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("mustRender panics on a template it does not have")
		}
	}()
	mustRender("nope.tmpl", nil)
}

func TestExecRunnerReturnsWhatTheCommandPrinted(t *testing.T) {
	out, err := ExecRunner(t.Context(), "sh", "-c", "echo out; echo err >&2")
	if err != nil || !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Fatalf("got %q %v", out, err)
	}
	if _, err := ExecRunner(t.Context(), "sh", "-c", "exit 3"); err == nil {
		t.Fatal("a command that fails reports it")
	}
}
