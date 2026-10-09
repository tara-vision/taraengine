package llamaserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// processGone reports whether no process has the pid any more.
func processGone(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

// awaitProcessGone waits for the process to disappear. One that stays is killed, so a failed test leaves
// nothing behind.
func awaitProcessGone(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d is still alive after %v", pid, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// grandchildIn waits for the "grandchild <pid>" line a helper writes and returns the pid.
func grandchildIn(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		for _, line := range strings.Split(string(data), "\n") {
			var pid int
			if _, err := fmt.Sscanf(line, "grandchild %d", &pid); err == nil && pid > 0 {
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no grandchild line in %s", path)
	return 0
}

// servePreset gives the supervisor's child the preset argument a backend gives it, which names the data
// directory, and returns that path.
func servePreset(t *testing.T, s *Supervisor) string {
	t.Helper()
	preset := filepath.Join(t.TempDir(), "presets.ini")
	s.opts.Args = append([]string{"--models-preset", preset}, s.opts.Args...)
	return preset
}

// leftoverRouter starts a helper the way a dead daemon's supervisor left its router: under binary, with
// the preset argument, in a process group of its own, with a grandchild in that group. Its pid goes into
// the pid file. The test reaps it, as the reaper of an orphan would; the cleanup kills whatever is left.
func leftoverRouter(t *testing.T, binary, pidFile, preset string, env ...string) (pid, grandchild int, exited <-chan struct{}) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "leftover.log")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	cmd := exec.Command(binary, "--models-preset", preset, "--port", strconv.Itoa(freePort(t)))
	cmd.Env = append([]string{"TARAENGINE_HELPER=1", "HELPER_GRANDCHILD=1"}, env...)
	cmd.Stdout, cmd.Stderr = f, f
	ownProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-done })
	writeTestFile(t, pidFile, strconv.Itoa(cmd.Process.Pid)+"\n")
	return cmd.Process.Pid, grandchildIn(t, out), done
}

func TestSupervisorKeepsThePIDOfItsChildInThePIDFile(t *testing.T) {
	s, _, _ := helperSupervisor(t, "HELPER_EXIT_AFTER_MS=300", "HELPER_EXIT_CODE=3")
	s.opts.PIDFile = filepath.Join(t.TempDir(), "child.pid")
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := s.PID()
	data, err := os.ReadFile(s.opts.PIDFile)
	if err != nil || string(data) != strconv.Itoa(first)+"\n" {
		t.Fatalf("pid file %q, want %d: %v", data, first, err)
	}
	if info, _ := os.Stat(s.opts.PIDFile); info.Mode().Perm() != 0o600 {
		t.Fatalf("pid file mode %v", info.Mode().Perm())
	}
	nextEvents(t, s, 1)
	eventually(t, 10*time.Second, "the restart to rewrite the pid file", func() bool {
		data, _ := os.ReadFile(s.opts.PIDFile)
		pid := s.PID()
		return pid != 0 && pid != first && string(data) == strconv.Itoa(pid)+"\n"
	})
	stopAndWait(t, s)
	if _, err := os.Stat(s.opts.PIDFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a clean stop removes the pid file: %v", err)
	}
}

func TestSupervisorFailedStartRemovesThePIDFile(t *testing.T) {
	s, _, _ := helperSupervisor(t, "HELPER_NO_HEALTH=1")
	s.opts.PIDFile = filepath.Join(t.TempDir(), "child.pid")
	s.opts.StartTimeout = 200 * time.Millisecond
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("want a start timeout")
	}
	if _, err := os.Stat(s.opts.PIDFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a child that was killed and reaped leaves no pid file: %v", err)
	}
}

func TestSupervisorLogsAPIDFileItCannotWrite(t *testing.T) {
	s, _, _ := helperSupervisor(t)
	rec := &logRecorder{}
	s.opts.Logger = slog.New(rec)
	s.opts.PIDFile = filepath.Join(t.TempDir(), "absent", "child.pid")
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("a pid file that cannot be written must not stop the child: %v", err)
	}
	if !rec.Contains("could not write the pid file") {
		t.Fatalf("lines %v", rec.lines)
	}
}

func TestSupervisorEndsARouterAnEarlierDaemonLeftBehind(t *testing.T) {
	s, _, _ := helperSupervisor(t)
	link, _ := linkToTestBinary(t)
	s.opts.Binary = link
	s.opts.PIDFile = filepath.Join(t.TempDir(), "child.pid")
	rec := &logRecorder{}
	s.opts.Logger = slog.New(rec)
	pid, grandchild, exited := leftoverRouter(t, link, s.opts.PIDFile, servePreset(t, s))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second): // the reaper may need a moment after the group is gone
		t.Fatal("the leftover router must be gone before the new one starts")
	}
	awaitProcessGone(t, grandchild, 5*time.Second)
	if !rec.Contains("left over from an earlier run pid=" + strconv.Itoa(pid)) {
		t.Fatalf("the leftover must be logged with its pid: %v", rec.lines)
	}
	if s.PID() == pid || !s.Running() {
		t.Fatalf("the new child runs: pid %d", s.PID())
	}
}

func TestSupervisorKillsALeftoverThatIgnoresTerm(t *testing.T) {
	s, _, _ := helperSupervisor(t)
	link, _ := linkToTestBinary(t)
	s.opts.Binary = link
	s.opts.PIDFile = filepath.Join(t.TempDir(), "child.pid")
	s.opts.Grace = 300 * time.Millisecond
	_, grandchild, exited := leftoverRouter(t, link, s.opts.PIDFile, servePreset(t, s), "HELPER_IGNORE_TERM=1", "HELPER_EXIT_AFTER_MS=20000")
	began := time.Now()
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took < 300*time.Millisecond {
		t.Fatalf("SIGKILL comes only after the grace, the start took %v", took)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the leftover that ignores SIGTERM survived")
	}
	awaitProcessGone(t, grandchild, 5*time.Second)
}

// signalRecorder passes signals on, except those its refuse function refuses, and keeps the list.
type signalRecorder struct {
	mu     sync.Mutex
	sent   []string
	refuse func(pid int, sig syscall.Signal) error
}

func (r *signalRecorder) send(pid int, sig syscall.Signal) error {
	r.mu.Lock()
	r.sent = append(r.sent, fmt.Sprintf("%d %d", pid, sig))
	r.mu.Unlock()
	if r.refuse != nil {
		if err := r.refuse(pid, sig); err != nil {
			return err
		}
	}
	return signalGroup(pid, sig)
}

func (r *signalRecorder) to(pid int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, line := range r.sent {
		if strings.HasPrefix(line, strconv.Itoa(pid)+" ") {
			n++
		}
	}
	return n
}

func TestSupervisorLeavesAPIDFileOfAnythingElseAlone(t *testing.T) {
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	stranger := exec.Command("sleep", "30")
	if err := stranger.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stranger.Process.Kill(); _ = stranger.Wait() })
	contents := map[string]string{
		"a process that runs another binary": strconv.Itoa(stranger.Process.Pid) + "\n",
		"a process that is gone":             strconv.Itoa(gone.Process.Pid) + "\n",
		"no number":                          "llama-server\n",
		"the init process":                   "1\n",
	}
	for name, content := range contents {
		t.Run(name, func(t *testing.T) {
			s, _, _ := helperSupervisor(t)
			servePreset(t, s) // so the check gets past the preset argument to the pid file's content
			rec := &signalRecorder{}
			s.sendSignal = rec.send
			s.opts.PIDFile = filepath.Join(t.TempDir(), "child.pid")
			writeTestFile(t, s.opts.PIDFile, content)
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := rec.to(stranger.Process.Pid); n != 0 {
				t.Fatalf("%d signals went to a process that is no router of ours", n)
			}
		})
	}
	if processGone(stranger.Process.Pid) {
		t.Fatal("the stranger was killed")
	}
}

func TestSupervisorLogsALeftoverItCannotEnd(t *testing.T) {
	s, _, _ := helperSupervisor(t)
	link, _ := linkToTestBinary(t)
	s.opts.Binary = link
	s.opts.PIDFile = filepath.Join(t.TempDir(), "child.pid")
	log := &logRecorder{}
	s.opts.Logger = slog.New(log)
	pid, _, _ := leftoverRouter(t, link, s.opts.PIDFile, servePreset(t, s))
	rec := &signalRecorder{refuse: func(to int, _ syscall.Signal) error {
		if to == pid {
			return syscall.EPERM
		}
		return nil
	}}
	s.sendSignal = rec.send
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("a leftover that cannot be ended must not stop the start: %v", err)
	}
	if !log.Contains("is still there pid=" + strconv.Itoa(pid)) {
		t.Fatalf("lines %v", log.lines)
	}
	if processGone(pid) {
		t.Fatal("the refused signals reached the leftover")
	}
}

// The router died; the model process it started holds the log pipe and would hold the GPU. Wait must not
// hang on the pipe, and the group must be gone before the backoff starts.
func TestSupervisorEndsTheGroupOfAChildThatExited(t *testing.T) {
	s, _, logPath := helperSupervisor(t, "HELPER_GRANDCHILD=1", "HELPER_EXIT_AFTER_MS=300", "HELPER_EXIT_CODE=3")
	s.waitDelay = 300 * time.Millisecond
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	grandchild := grandchildIn(t, logPath)
	if e := nextEvents(t, s, 1)[0]; e.Code != 3 {
		t.Fatalf("event %+v", e)
	}
	awaitProcessGone(t, grandchild, 2*time.Second)
}

func TestSupervisorLogsAGroupItCannotEnd(t *testing.T) {
	s, _, _ := helperSupervisor(t, "HELPER_EXIT_AFTER_MS=300", "HELPER_EXIT_CODE=3")
	log := &logRecorder{}
	s.opts.Logger = slog.New(log)
	rec := &signalRecorder{refuse: func(_ int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			return syscall.EPERM
		}
		return nil
	}}
	s.sendSignal = rec.send
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nextEvents(t, s, 1)
	first := log.startedPIDs()[0]
	if !log.Contains("of the exited llama-server pid=" + strconv.Itoa(first)) {
		t.Fatalf("lines %v", log.lines)
	}
}

func TestSupervisorLeavesARouterOfAnotherDataDirectoryAlone(t *testing.T) {
	s, _, _ := helperSupervisor(t)
	link, _ := linkToTestBinary(t)
	s.opts.Binary = link
	s.opts.PIDFile = filepath.Join(t.TempDir(), "child.pid")
	servePreset(t, s)
	rec := &signalRecorder{}
	s.sendSignal = rec.send
	// The same runtime serves another data directory; a stale or copied pid file names its router.
	other := filepath.Join(t.TempDir(), "presets.ini")
	pid, _, exited := leftoverRouter(t, link, s.opts.PIDFile, other)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := rec.to(pid); n != 0 {
		t.Fatalf("%d signals went to the router of another data directory", n)
	}
	select {
	case <-exited:
		t.Fatal("the router of another data directory was ended")
	default:
	}
}

// leftoverWaiting starts a supervisor whose start finds a leftover that ignores SIGTERM, so the start waits
// out the grace, and returns the supervisor and its log.
func leftoverWaiting(t *testing.T) (*Supervisor, *logRecorder) {
	t.Helper()
	s, _, _ := helperSupervisor(t)
	link, _ := linkToTestBinary(t)
	s.opts.Binary = link
	s.opts.PIDFile = filepath.Join(t.TempDir(), "child.pid")
	rec := &logRecorder{}
	s.opts.Logger = slog.New(rec)
	leftoverRouter(t, link, s.opts.PIDFile, servePreset(t, s), "HELPER_IGNORE_TERM=1", "HELPER_EXIT_AFTER_MS=20000")
	return s, rec
}

func TestSupervisorStopEndsTheWaitForALeftover(t *testing.T) {
	s, rec := leftoverWaiting(t)
	started := make(chan error, 1)
	go func() { started <- s.Start(context.Background()) }()
	eventually(t, 5*time.Second, "the leftover to be signalled", func() bool { return rec.Contains("left over from an earlier run") })
	began := time.Now()
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recvErr(t, started, "Start"); !errors.Is(err, errStopping) {
		t.Fatalf("Start returned %v, want the stopping error", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("the stop took %v; it must end the wait for the leftover at once", took)
	}
	if n := rec.Count("llama-server started"); n != 0 {
		t.Fatalf("%d routers were started after the stop", n)
	}
}

func TestSupervisorStartGivesUpTheWaitForALeftoverWhenItsContextEnds(t *testing.T) {
	s, rec := leftoverWaiting(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err := s.Start(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start returned %v, want the context's error", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("the start took %v; the context must end the wait", took)
	}
	if n := rec.Count("llama-server started"); n != 0 {
		t.Fatalf("%d routers were started by a start whose caller gave up", n)
	}
}
