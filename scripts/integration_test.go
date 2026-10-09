package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeExecutable writes a script that every test below uses as a stand-in for a command.
func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec // a stand-in the test runs
		t.Fatal(err)
	}
}

// copyScript puts scripts/<name> into root/scripts, the layout the scripts expect.
func copyScript(t *testing.T, root, name string) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(root, "scripts", name), string(data))
}

// TestIntegrationScriptStopsWhenThereIsNoRuntimeDirectory runs the script with a binary that installs nothing. The
// script has to fail then: if it went on, the integration test would find no runtime and skip, and a job that runs
// nothing would pass.
func TestIntegrationScriptStopsWhenThereIsNoRuntimeDirectory(t *testing.T) {
	cases := []struct{ name, status string }{
		{"empty answer", "exit 0"},
		{"a directory that is not there", `echo "$TMPDIR_FOR_TEST/missing"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			copyScript(t, root, "integration.sh")
			writeExecutable(t, filepath.Join(root, "taraengine"), "#!/bin/sh\ncase \"$*\" in *--dir-only*) "+tc.status+" ;; esac\n")
			ran := filepath.Join(root, "go-ran")
			writeExecutable(t, filepath.Join(root, "bin", "go"), "#!/bin/sh\ntouch "+ran+"\n")

			cmd := exec.Command("bash", filepath.Join(root, "scripts", "integration.sh"))
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"),
				"TMPDIR_FOR_TEST="+root, "TARAENGINE_INTEGRATION_DIR=")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("the script went on without a runtime directory:\n%s", out)
			}
			if !strings.Contains(string(out), "runtime directory") {
				t.Fatalf("the failure does not name the runtime directory:\n%s", out)
			}
			if _, statErr := os.Stat(ran); statErr == nil {
				t.Fatal("go test ran although there was no runtime directory")
			}
		})
	}
}

// TestIntegrationScriptRunsTheTestWithTheRuntimeItInstalled is the other side: with a runtime directory, go test
// runs, and it gets the binary and the directory in the environment.
func TestIntegrationScriptRunsTheTestWithTheRuntimeItInstalled(t *testing.T) {
	root := t.TempDir()
	copyScript(t, root, "integration.sh")
	runtimeDir := filepath.Join(root, "runtime", "b1")
	if err := os.MkdirAll(runtimeDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(root, "taraengine"), "#!/bin/sh\ncase \"$*\" in *--dir-only*) echo "+runtimeDir+" ;; esac\n")
	seen := filepath.Join(root, "seen")
	writeExecutable(t, filepath.Join(root, "bin", "go"),
		"#!/bin/sh\necho \"$TARAENGINE_BIN $TARAENGINE_RUNTIME_DIR $*\" > "+seen+"\n")

	cmd := exec.Command("bash", filepath.Join(root, "scripts", "integration.sh"), "-run", "TestX")
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+":"+os.Getenv("PATH"), "TARAENGINE_INTEGRATION_DIR=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the script failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("go test never ran: %v", err)
	}
	want := filepath.Join(root, "taraengine") + " " + runtimeDir + " test -tags integration"
	if !strings.HasPrefix(string(got), want) || !strings.Contains(string(got), "-run TestX") {
		t.Fatalf("go test saw %q, want it to start with %q and carry the extra arguments", got, want)
	}
}
