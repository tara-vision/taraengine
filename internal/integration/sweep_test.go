//go:build integration

package integration

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPidsMentioningListsTheCommandLinesThatNameTheMarker(t *testing.T) {
	const dir = "/tmp/TestX/001/Application Support/taraengine"
	table := "    1 /sbin/launchd\n" +
		"  412 /opt/rt/llama-server --models-preset " + dir + "/presets.ini --port 51234\n" +
		"  413 /opt/rt/llama-server --model " + dir + "/models/m.gguf\n" +
		"  500 /usr/bin/other --data /tmp/TestX/001/Application Support/other\n" +
		"  600 taraengine serve --config " + dir + "/config.yaml\n" +
		"  700\n" +
		"not-a-pid " + dir + "\n" +
		"\n"
	got := pidsMentioning(table, dir, 600)
	if want := []int{412, 413}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := pidsMentioning(table, ""); len(got) != 0 {
		t.Fatalf("an empty marker must name no process, got %v", got)
	}
}

func TestStraysFindsWhatIsLeftOfADaemonByItsDataDirectory(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "Application Support", "taraengine")
	// A process whose command line names the data directory, the way the router of a daemon does.
	cmd := exec.Command("bash", "-c", `exec -a "$0" sleep 60`, "stray "+filepath.Join(dataDir, "presets.ini"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	d := &daemon{dataDir: dataDir}
	if got := d.strays(); !slices.Contains(got, cmd.Process.Pid) {
		t.Fatalf("strays %v lack the process %d that names %s", got, cmd.Process.Pid, dataDir)
	}
	other := &daemon{dataDir: filepath.Join(t.TempDir(), "taraengine")}
	if got := other.strays(); slices.Contains(got, cmd.Process.Pid) || slices.Contains(got, os.Getpid()) {
		t.Fatalf("strays of another data directory: %v", got)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	awaitGone(t, []int{cmd.Process.Pid})
	if got := d.strays(); slices.Contains(got, cmd.Process.Pid) {
		t.Fatalf("a killed process is still listed: %v", got)
	}
}

// sweepChildMarker carries the data directory to TestSweepChild.
const sweepChildMarker = "INTEGRATION_SWEEP_CHILD"

var strayLine = regexp.MustCompile(`stray (\d+)`)

// TestSweepChild is the end of a test whose daemon died on its own while a process of its runtime lived on. It starts
// that process, which no longer descends from anything of ours, announces its id and lets the cleanup of the daemon
// run. On its own it does nothing.
func TestSweepChild(t *testing.T) {
	dataDir := os.Getenv(sweepChildMarker)
	if dataDir == "" {
		t.Skip("runs only as the child of TestCleanupSweepsTheRuntimeOfADaemonThatDiedAlone")
	}
	d := &daemon{dataDir: dataDir, done: make(chan struct{}), waitErr: errors.New("signal: killed")}
	close(d.done)
	t.Cleanup(func() { d.cleanup(t) })
	// The inner shell is started in the background and the outer one exits at once, so the process that stays is an
	// orphan, like the router of a daemon that was killed.
	out, err := exec.Command("bash", "-c", `bash -c 'exec -a "$0" sleep 60' "$1" >/dev/null 2>&1 & echo $!`,
		"_", "stray "+filepath.Join(dataDir, "presets.ini")).Output()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("stray %s", out)
}

func TestCleanupSweepsTheRuntimeOfADaemonThatDiedAlone(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "Application Support", "taraengine")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSweepChild$", "-test.v")
	cmd.Env = append(childEnvironment(), sweepChildMarker+"="+dataDir)
	out, err := cmd.CombinedOutput()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 1 || !strings.Contains(string(out), "had already exited") {
		t.Fatalf("the daemon that died alone must fail its test: %v\n%s", err, out)
	}
	m := strayLine.FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("the child never started its stray process:\n%s", out)
	}
	pid, _ := strconv.Atoi(m[1])
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(5 * time.Second)
	for processExists(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the cleanup left process %d of the runtime running", pid)
		}
		time.Sleep(pollEvery)
	}
}
