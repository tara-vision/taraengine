package llamaserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// parkedStart is a Start whose health probe waits until the test releases it.
type parkedStart struct {
	result  chan error
	release func()
}

// startWithParkedProbe runs Start in the background and returns once its health probe is waiting, with
// the child already spawned. The cleanup releases the probe, so a failing test cannot hang its Stop.
func startWithParkedProbe(t *testing.T, s *Supervisor) *parkedStart {
	t.Helper()
	probing := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	releaseProbe := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseProbe)
	s.opts.Health = func(context.Context) error {
		select {
		case probing <- struct{}{}:
		default:
		}
		<-release
		return nil
	}
	result := make(chan error, 1)
	go func() { result <- s.Start(context.Background()) }()
	select {
	case <-probing:
	case <-time.After(10 * time.Second):
		t.Fatal("Start never reached its health probe")
	}
	return &parkedStart{result: result, release: releaseProbe}
}

func TestSupervisorStopWithoutStart(t *testing.T) {
	s, _, _ := helperSupervisor(t)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("stop of a supervisor that never started: %v", err)
	}
	if s.Running() || s.PID() != 0 {
		t.Fatalf("running=%v pid=%d", s.Running(), s.PID())
	}
}

// A stopped supervisor is finished: a later Start is refused before anything is forked.
func TestSupervisorStartAfterStopIsRefused(t *testing.T) {
	s, _, _ := helperSupervisor(t)
	rec := &logRecorder{}
	s.opts.Logger = slog.New(rec)
	ctx := context.Background()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := s.Start(ctx); !errors.Is(err, errStopping) {
		t.Fatalf("want the stopping error from Start after Stop, got %v", err)
	}
	if got := rec.Count("llama-server started"); got != 0 {
		t.Fatalf("%d children were started after the stop, want none", got)
	}
	if s.Running() || s.PID() != 0 {
		t.Fatalf("running=%v pid=%d after the refused start", s.Running(), s.PID())
	}
}

// Stop has to wait for a Start that is still waiting for health. That Start then fails with the stopping
// error and kills its child, even a child that ignores SIGTERM.
func TestSupervisorStopDuringStartLeavesNoChild(t *testing.T) {
	s, _, _ := helperSupervisor(t, "HELPER_NO_HEALTH=1", "HELPER_IGNORE_TERM=1", "HELPER_EXIT_AFTER_MS=20000")
	s.opts.Grace = 300 * time.Millisecond
	parked := startWithParkedProbe(t, s)

	stopped := make(chan error, 1)
	go func() { stopped <- s.Stop(context.Background()) }()
	waitStopping(t, s)
	select {
	case err := <-stopped:
		t.Fatalf("stop returned while Start was still in flight: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	parked.release() // the probe now answers "healthy", after the stop landed
	if err := recvErr(t, parked.result, "Start"); !errors.Is(err, errStopping) {
		t.Fatalf("Start returned %v, want the stopping error", err)
	}
	if err := recvErr(t, stopped, "Stop"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if s.Running() || s.PID() != 0 {
		t.Fatalf("after stop: running=%v pid=%d", s.Running(), s.PID())
	}
	time.Sleep(200 * time.Millisecond)
	if helperAlive(t, helperPort(s)) {
		t.Fatal("the child that ignores SIGTERM is still alive")
	}
}

// A Stop whose context ends while it waits for Start must not leave the starting child behind.
func TestSupervisorStopContextEndsWhileStartIsInFlight(t *testing.T) {
	s, _, _ := helperSupervisor(t, "HELPER_NO_HEALTH=1", "HELPER_IGNORE_TERM=1", "HELPER_EXIT_AFTER_MS=20000")
	parked := startWithParkedProbe(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := s.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop returned %v, want the context deadline", err)
	}
	time.Sleep(200 * time.Millisecond)
	if helperAlive(t, helperPort(s)) {
		t.Fatal("the starting child survived a stop whose context ended")
	}
	parked.release()
	if err := recvErr(t, parked.result, "Start"); !errors.Is(err, errStopping) {
		t.Fatalf("Start returned %v, want the stopping error", err)
	}
}

func TestSupervisorSecondStartIsRefused(t *testing.T) {
	s, _, _ := helperSupervisor(t, "HELPER_NO_HEALTH=1", "HELPER_EXIT_AFTER_MS=20000")
	rec := &logRecorder{}
	s.opts.Logger = slog.New(rec)
	parked := startWithParkedProbe(t, s)

	second := make(chan error, 1)
	go func() { second <- s.Start(context.Background()) }()
	if err := recvErr(t, second, "the second Start"); !errors.Is(err, errAlreadyStarted) {
		t.Fatalf("a Start while another is in flight returned %v", err)
	}
	parked.release()
	if err := recvErr(t, parked.result, "Start"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, errAlreadyStarted) {
		t.Fatalf("a Start after a successful one returned %v", err)
	}
	if got := rec.Count("llama-server started"); got != 1 {
		t.Fatalf("%d children were started, want 1", got)
	}
}

// SIGTERM is ignored, so only the SIGKILL that follows the grace can end the child.
func TestSupervisorStopKillsAChildThatIgnoresTerm(t *testing.T) {
	s, _, _ := helperSupervisor(t, "HELPER_IGNORE_TERM=1", "HELPER_EXIT_AFTER_MS=20000")
	const grace = 300 * time.Millisecond
	s.opts.Grace = grace
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	began := time.Now()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if took := time.Since(began); took < grace || took > 3*time.Second {
		t.Fatalf("stop took %v; SIGKILL should follow the %v grace", took, grace)
	}
	if s.Running() || s.PID() != 0 {
		t.Fatalf("after stop: running=%v pid=%d", s.Running(), s.PID())
	}
	time.Sleep(200 * time.Millisecond)
	if helperAlive(t, helperPort(s)) {
		t.Fatal("the child that ignores SIGTERM is still alive")
	}
}

// When the stop context ends first the child is killed at once; the watcher then finishes by itself and
// closes the log.
func TestSupervisorStopContextEndsWithARunningChild(t *testing.T) {
	s, _, _ := helperSupervisor(t, "HELPER_IGNORE_TERM=1", "HELPER_EXIT_AFTER_MS=20000")
	spy := &logSpy{}
	s.newLog = func(string) io.WriteCloser { return spy }
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := s.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop returned %v, want the context deadline", err)
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never finished after the child was killed")
	}
	if got := spy.Closes(); got != 1 {
		t.Fatalf("the log was closed %d times, want 1", got)
	}
	time.Sleep(200 * time.Millisecond)
	if helperAlive(t, helperPort(s)) {
		t.Fatal("the child is still alive after the stop context ended")
	}
}

func TestSupervisorFailedStartClosesTheLog(t *testing.T) {
	s, _, _ := helperSupervisor(t, "HELPER_NO_HEALTH=1")
	spy := &logSpy{}
	s.newLog = func(string) io.WriteCloser { return spy }
	s.opts.StartTimeout = 300 * time.Millisecond
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("want a start timeout")
	}
	if got := spy.Closes(); got != 1 {
		t.Fatalf("the log was closed %d times after a failed start, want 1", got)
	}
}

// The watcher closes the log when it ends, so giving up releases the file before any Stop.
func TestSupervisorGivingUpClosesTheLog(t *testing.T) {
	s, _, _ := helperSupervisor(t, "HELPER_EXIT_AFTER_MS=1", "HELPER_EXIT_CODE=1")
	spy := &logSpy{}
	s.newLog = func(string) io.WriteCloser { return spy }
	s.opts.Health = func(context.Context) error { return nil } // the quick exits are the point, not the probe
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	nextEvents(t, s, 5)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never finished after giving up")
	}
	if got := spy.Closes(); got != 1 {
		t.Fatalf("the log was closed %d times when the watcher ended, want 1", got)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := spy.Closes(); got != 1 {
		t.Fatalf("the log was closed %d times after Stop, want 1", got)
	}
}

func TestSupervisorStartFailsWhenTheBinaryIsMissing(t *testing.T) {
	s, _, _ := helperSupervisor(t)
	spy := &logSpy{}
	s.newLog = func(string) io.WriteCloser { return spy }
	rec := &logRecorder{}
	s.opts.Logger = slog.New(rec)
	s.opts.Binary = filepath.Join(t.TempDir(), "no-such-llama-server")
	if err := s.Start(context.Background()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Start = %v, want a missing-binary error", err)
	}
	if got := spy.Closes(); got != 1 {
		t.Fatalf("the log was closed %d times, want 1", got)
	}
	if got := rec.Count("llama-server started"); got != 0 {
		t.Fatalf("%d children were started, want none", got)
	}
	// the failed start gave the Start slot back
	if err := s.Start(context.Background()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a second Start after the failure returned %v, want the missing-binary error again", err)
	}
}

func TestSupervisorStartCreatesTheLogDirectoryPrivately(t *testing.T) {
	s, _, _ := helperSupervisor(t)
	root := t.TempDir()
	s.opts.LogPath = filepath.Join(root, "logs", "engine", "llama-server.log")
	s.opts.Binary = filepath.Join(root, "missing-binary") // the start fails at the spawn, after the directory exists
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("want a start failure for the missing binary")
	}
	for _, dir := range []string{filepath.Join(root, "logs"), filepath.Join(root, "logs", "engine")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("log directory: %v", err)
		}
		perm := info.Mode().Perm()
		if perm&0o700 != 0o700 || perm&^0o750 != 0 {
			t.Fatalf("%s has mode %o, want the owner's access and nothing beyond 0750", dir, perm)
		}
	}
}

func TestSupervisorStartReportsALogDirectoryItCannotCreate(t *testing.T) {
	s, _, _ := helperSupervisor(t)
	rec := &logRecorder{}
	s.opts.Logger = slog.New(rec)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("a file where a directory should be"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.opts.LogPath = filepath.Join(blocker, "logs", "llama-server.log")
	err := s.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "log directory") {
		t.Fatalf("Start = %v, want an error about the log directory", err)
	}
	if got := rec.Count("llama-server started"); got != 0 {
		t.Fatalf("%d children were started, want none", got)
	}
}

func TestSupervisorStartRejectsMissingOptions(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*SupervisorOptions)
		want   string
	}{
		{"binary", func(o *SupervisorOptions) { o.Binary = "" }, "Binary"},
		{"log path", func(o *SupervisorOptions) { o.LogPath = "" }, "LogPath"},
		{"health probe", func(o *SupervisorOptions) { o.Health = nil }, "Health"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := SupervisorOptions{
				Binary:  "sleep",
				Args:    []string{"1"},
				LogPath: filepath.Join(t.TempDir(), "llama-server.log"),
				Health:  func(context.Context) error { return nil },
			}
			tc.mutate(&opts)
			s := NewSupervisor(opts)
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Start panicked: %v", r)
					}
				}()
				err = s.Start(context.Background())
			}()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Start = %v, want an error naming %s", err, tc.want)
			}
			if s.Running() {
				t.Fatal("a child was started although the options are incomplete")
			}
		})
	}
}

func TestSignalGroupRefusesPidsThatReachTheWrongProcesses(t *testing.T) {
	// Signal 0 only checks permissions, so even a missing guard could not hurt this test run.
	for _, pid := range []int{1, 0, -1, -4242} {
		if err := signalGroup(pid, syscall.Signal(0)); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("signalGroup(%d) = %v, want a refusal", pid, err)
		}
	}
}

func TestSupervisorSignalRefusesAChildWithoutAPid(t *testing.T) {
	rec := &logRecorder{}
	s := NewSupervisor(SupervisorOptions{Logger: slog.New(rec)})
	// pid 0 would make the kill reach the daemon's own process group
	s.signal(&child{cmd: &exec.Cmd{Process: &os.Process{Pid: 0}}}, syscall.Signal(0))
	if !rec.Contains("refusing") {
		t.Fatalf("a child with pid 0 was not refused: %v", rec.lines)
	}
}

func TestSupervisorSignalSkipsExitedChildrenAndLogsRealErrors(t *testing.T) {
	rec := &logRecorder{}
	s := NewSupervisor(SupervisorOptions{Logger: slog.New(rec)})
	calls := 0
	var fail error
	s.sendSignal = func(int, syscall.Signal) error {
		calls++
		return fail
	}
	live := &child{cmd: &exec.Cmd{Process: &os.Process{Pid: 4242}}}
	gone := &child{cmd: &exec.Cmd{Process: &os.Process{Pid: 4243}}}
	gone.exited.Store(true)

	s.signal(nil, syscall.SIGTERM)
	s.signal(gone, syscall.SIGKILL)
	if calls != 0 {
		t.Fatalf("%d signals were sent to a missing or exited child, want none", calls)
	}

	fail = fmt.Errorf("kill: %w", syscall.ESRCH) // the group is already gone: nothing to report
	s.signal(live, syscall.SIGTERM)
	if calls != 1 || rec.Count("llama-server signal failed") != 0 {
		t.Fatalf("calls=%d, a vanished group must not be logged: %v", calls, rec.lines)
	}

	fail = syscall.EPERM // anything else is worth a line
	s.signal(live, syscall.SIGTERM)
	if rec.Count("llama-server signal failed") != 1 || !rec.Contains("pid=4242") {
		t.Fatalf("a real signal error was not logged: %v", rec.lines)
	}
}

// A model process that ignores SIGTERM and keeps the log pipe open outlives the router on a stop. The wait
// delay ends the watcher well before the grace would bring SIGKILL, so the stop must end the group itself.
func TestSupervisorStopEndsAModelProcessThatIgnoresTerm(t *testing.T) {
	s, _, logPath := helperSupervisor(t, "HELPER_GRANDCHILD=1", "HELPER_GRANDCHILD_IGNORES_TERM=1")
	s.waitDelay = 200 * time.Millisecond
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	grandchild := grandchildIn(t, logPath)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitProcessGone(t, grandchild, 2*time.Second)
}
