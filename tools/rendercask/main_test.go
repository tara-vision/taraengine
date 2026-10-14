package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sums is a checksums.txt of a release, with the macOS binary among the other assets.
const sums = `51704cd82e8642186022ec7b25ab462aa28d875fb75a53b8608c995c8ae436c6  install.sh
424db42116bed4e38b5a579868a28fb9a6d74d3761610170541ed203e8f5dd18  taraengine-darwin-arm64
3020e4cd26e5c247368303f63eea64153a331106144fb9e7add2f03cfca780fb  taraengine-linux-amd64
`

func writeSums(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "checksums.txt")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func render(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestTheCaskIsTheGoldenOne(t *testing.T) {
	code, out, errOut := render(t, writeSums(t, sums), "v0.3.0")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	golden := filepath.Join("testdata", "taraengine.rb")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil || out != string(want) {
		t.Fatalf("%v; the cask differs from %s:\n%s", err, golden, out)
	}
}

func TestTheCaskIsRuby(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("ruby is required on CI: %v", err)
		}
		t.Skip("ruby is not installed")
	}
	_, out, _ := render(t, writeSums(t, sums), "0.3.0")
	path := filepath.Join(t.TempDir(), "taraengine.rb")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := exec.Command(ruby, "-c", path).CombinedOutput(); err != nil || !strings.Contains(string(got), "Syntax OK") {
		t.Fatalf("%v\n%s", err, got)
	}
}

func TestRenderRefusesWhatIsNoStableRelease(t *testing.T) {
	path := writeSums(t, sums)
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "usage"},
		{[]string{path}, 2, "usage"},
		{[]string{path, "0.3.0-rc.1"}, 1, "a stable release"},
		{[]string{path, "dev"}, 1, "a stable release"},
		{[]string{"-prerelease", path, "dev"}, 1, "a stable release"},
		{[]string{"-nope", path, "0.3.0"}, 2, "usage"},
		{[]string{filepath.Join(t.TempDir(), "missing.txt"), "0.3.0"}, 1, "missing.txt"},
		{[]string{writeSums(t, "abc  taraengine-linux-amd64\n"), "0.3.0"}, 1, "no sha256 for taraengine-darwin-arm64"},
	} {
		code, out, errOut := render(t, c.args...)
		if code != c.code || out != "" || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: exit %d, %q, %q", c.args, code, out, errOut)
		}
	}
}

func TestMainExitsWithTheStatusOfRun(t *testing.T) {
	previous, previousArgs, status := exit, os.Args, -1
	exit = func(code int) { status = code }
	t.Cleanup(func() { exit, os.Args = previous, previousArgs })
	os.Args = []string{"rendercask"}
	main()
	if status != 2 {
		t.Fatalf("got %d", status)
	}
}

// refusingWriter fails every write, as a closed pipe would.
type refusingWriter struct{}

func (refusingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func TestRenderReportsAnOutputItCannotWrite(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{writeSums(t, sums), "0.3.0"}, refusingWriter{}, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "file already closed") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestAPrereleaseCaskIsForALocalTapOnly(t *testing.T) {
	code, out, errOut := render(t, "-prerelease", writeSums(t, sums), "v0.3.0-rc.1")
	if code != 0 || !strings.Contains(out, `version "0.3.0-rc.1"`) {
		t.Fatalf("exit %d: %s\n%s", code, errOut, out)
	}
}
