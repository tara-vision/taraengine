//go:build integration

package integration

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// succeeded returns the output of a command that exited 0 and wrote nothing to stderr, and fails the test otherwise.
func succeeded(t *testing.T, res cliResult, what string) string {
	t.Helper()
	if res.code != 0 || res.stderr != "" {
		t.Fatalf("%s: exit %d, stderr %q, stdout %q", what, res.code, res.stderr, res.stdout)
	}
	return res.stdout
}

// rowFor is the fields of the line of the output whose first field is name.
func rowFor(t *testing.T, output, name string) []string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == name {
			return fields
		}
	}
	t.Fatalf("no row for %s in:\n%s", name, output)
	return nil
}

// checkCLI runs the commands of the built binary against the daemon while the model is loaded, and asserts what they
// print. It also asserts the one thing only the binary decides: that an error is a line on stderr behind
// "taraengine: " and the exit status 1.
func checkCLI(t *testing.T, s *scenario) {
	v := s.version(t)
	window := strconv.Itoa(contextWindow)
	if got, want := succeeded(t, s.d.cli(t, "version"), "version"), "taraengine "+v.Version+"\n"; got != want {
		t.Fatalf("version printed %q, want %q", got, want)
	}

	row := rowFor(t, succeeded(t, s.d.cli(t, "list"), "list"), s.entry.Name)
	if row[1] != "loaded" || !slices.Contains(row, "small") || !slices.Contains(row, window) {
		t.Fatalf("list shows %v", row)
	}

	row = rowFor(t, succeeded(t, s.d.cli(t, "ps"), "ps"), s.entry.Name)
	if source := row[len(row)-1]; row[1] != "loaded" || row[2] != window || !slices.Contains(row, "GiB") ||
		(source != "measured" && source != "estimated") {
		t.Fatalf("ps shows %v", row)
	}

	out := succeeded(t, s.d.cli(t, "doctor"), "doctor")
	for _, want := range []string{
		"Machine: ",
		"Daemon: taraengine " + v.Version + " on " + v.Platform + ", runtime " + v.Runtime.Build + ", at " + s.d.baseURL,
		fmt.Sprintf("Loaded: %s at %d tokens", s.entry.Name, contextWindow),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor does not say %q:\n%s", want, out)
		}
	}

	res := s.d.cli(t, "load", "no-such-model")
	if res.code != 1 || res.stdout != "" || !strings.HasPrefix(res.stderr, "taraengine: ") ||
		!strings.Contains(res.stderr, "no-such-model") || strings.Count(res.stderr, "\n") != 1 {
		t.Fatalf("a refused load printed %+v", res)
	}
}

// checkStopIdle stops a daemon that has no model loaded: below it there is the runtime's router.
func checkStopIdle(t *testing.T, s *scenario) { assertCleanStop(t, s, 1) }

// checkStopLoaded stops a daemon with a model loaded: below it there are the router and the instance that runs the
// model, and the stop has to take both.
func checkStopLoaded(t *testing.T, s *scenario) { assertCleanStop(t, s, 2) }

// assertCleanStop asserts that SIGTERM ends the daemon with status 0 in time, that at least wantBelow processes lived
// below it and none of them outlives it, that it logged no error, and that its address no longer answers.
func assertCleanStop(t *testing.T, s *scenario, wantBelow int) {
	sd := s.d.stop()
	if sd.err != nil {
		t.Fatalf("serve did not stop cleanly: %v\n%s", sd.err, s.d.logs)
	}
	if len(sd.tree) < wantBelow {
		t.Fatalf("expected at least %d processes below the daemon when it was stopped, saw %v", wantBelow, sd.tree)
	}
	awaitGone(t, sd.tree)
	if lines := s.d.errorLines(); len(lines) > 0 {
		t.Fatalf("the daemon logged errors:\n%s", strings.Join(lines, "\n"))
	}
	res := s.d.cli(t, "ps")
	if res.code != 1 || res.stdout != "" || !strings.HasPrefix(res.stderr, "taraengine: ") ||
		!strings.Contains(res.stderr, "is not running") {
		t.Fatalf("after the stop, ps printed %+v", res)
	}
}

// goneTimeout bounds how long the processes below a stopped daemon may take to disappear.
const goneTimeout = 15 * time.Second

// awaitGone waits for the processes to disappear. Any that stay are killed, so that a failed test leaves none behind.
func awaitGone(t *testing.T, pids []int) {
	t.Helper()
	deadline := time.Now().Add(goneTimeout)
	for {
		var alive []int
		for _, pid := range pids {
			if processExists(pid) {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, pid := range alive {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			t.Fatalf("processes of the runtime outlived the daemon: %v", alive)
		}
		time.Sleep(pollEvery)
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
