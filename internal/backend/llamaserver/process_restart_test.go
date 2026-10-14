package llamaserver

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A restart that cannot spawn is an exit like any other: it is reported, backs off, and adds up to the
// unhealthy verdict.
func TestSupervisorFailedSpawnsCountAsExitsAndEndUnhealthy(t *testing.T) {
	s, clock, _ := helperSupervisor(t, "HELPER_EXIT_AFTER_MS=300", "HELPER_EXIT_CODE=3")
	link, _ := linkToTestBinary(t)
	s.opts.Binary = link
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := os.Remove(link); err != nil { // from now on every restart fails to spawn
		t.Fatal(err)
	}
	events := nextEvents(t, s, 5)
	if events[0].Code != 3 || events[0].Err != nil {
		t.Fatalf("the child's own exit was reported as %+v", events[0])
	}
	for i, e := range events[1:] {
		if e.Code != -1 || !errors.Is(e.Err, fs.ErrNotExist) {
			t.Fatalf("failed spawn %d was reported as %+v, want code -1 and the spawn error", i+1, e)
		}
	}
	if !s.Unhealthy() || s.Running() {
		t.Fatalf("after five exits: unhealthy=%v running=%v", s.Unhealthy(), s.Running())
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	got := clock.Sleeps()
	if len(got) != len(want) {
		t.Fatalf("backoff sleeps %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff sleeps %v, want %v", got, want)
		}
	}
}

// A spawn that fails once is retried after the next backoff, and supervision goes on.
func TestSupervisorRetriesAFailedSpawn(t *testing.T) {
	s, clock, _ := helperSupervisor(t, "HELPER_EXIT_AFTER_MS=300", "HELPER_EXIT_CODE=3")
	link, target := linkToTestBinary(t)
	s.opts.Binary = link
	rec := &logRecorder{}
	s.opts.Logger = slog.New(rec)
	calls := 0 // only the watcher sleeps, so no lock is needed
	s.opts.Sleep = func(ctx context.Context, d time.Duration) error {
		if err := clock.Sleep(ctx, d); err != nil {
			return err
		}
		calls++
		switch calls {
		case 1: // the first child died: make the restart fail to spawn
			return os.Remove(link)
		case 2: // that spawn failed: bring the binary back before the retry
			return os.Symlink(target, link)
		}
		return nil
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	events := nextEvents(t, s, 2)
	if events[0].Code != 3 || events[0].Err != nil {
		t.Fatalf("the child's own exit was reported as %+v", events[0])
	}
	if events[1].Code != -1 || !errors.Is(events[1].Err, fs.ErrNotExist) {
		t.Fatalf("the failed spawn was reported as %+v", events[1])
	}
	eventually(t, 5*time.Second, "the retry to start a child", func() bool {
		return rec.Count("llama-server started") == 2
	})
	if got := clock.Sleeps(); len(got) < 2 || got[0] != time.Second || got[1] != 2*time.Second {
		t.Fatalf("backoff sleeps %v, want 1s then 2s", got)
	}
	if s.Unhealthy() {
		t.Fatal("two exits are not an unhealthy supervisor")
	}
}

// A restart that never answers its health probes is killed when the start timeout passes and counts
// as an exit.
func TestSupervisorCountsARestartThatNeverBecomesHealthy(t *testing.T) {
	s, _, _ := helperSupervisor(t, "HELPER_NO_HEALTH=1", "HELPER_EXIT_AFTER_MS=20000")
	rec := &logRecorder{}
	s.opts.Logger = slog.New(rec)
	var probes atomic.Int32
	s.opts.Health = func(context.Context) error {
		if probes.Add(1) == 1 {
			return nil // the first start is healthy
		}
		return errors.New("still loading") // every restart stays unhealthy
	}
	s.opts.StartTimeout = 100 * time.Millisecond
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := signalGroup(s.PID(), syscall.SIGKILL); err != nil { // the child crashes
		t.Fatal(err)
	}
	for i, e := range nextEvents(t, s, 5) {
		if e.Code != -1 || e.Err != nil {
			t.Fatalf("exit %d was reported as %+v, want a child killed by a signal", i+1, e)
		}
	}
	if !s.Unhealthy() {
		t.Fatal("five exits must end unhealthy")
	}
	if got := rec.Count("llama-server restart unhealthy"); got != 4 {
		t.Fatalf("%d restarts were judged unhealthy, want all 4", got)
	}
}

func newClockedSupervisor() (*Supervisor, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	return NewSupervisor(SupervisorOptions{Now: clock.Now}), clock
}

func TestRecordExitForgetsExitsOlderThanTheWindow(t *testing.T) {
	s, clock := newClockedSupervisor()
	for i := 1; i <= 4; i++ {
		if n := s.recordExit(exitInfo{code: 1}); n != i {
			t.Fatalf("exit %d: the window holds %d", i, n)
		}
		clock.Advance(time.Second)
	}
	clock.Advance(11 * time.Minute)
	if n := s.recordExit(exitInfo{code: 1}); n != 1 {
		t.Fatalf("after eleven quiet minutes the window holds %d exits, want only the new one", n)
	}
	if s.Unhealthy() {
		t.Fatal("old exits must not add up to an unhealthy supervisor")
	}
}

func TestRecordExitWindowEdge(t *testing.T) {
	cases := []struct {
		name string
		age  time.Duration
		want int
	}{
		{"an exit exactly ten minutes old drops out", unhealthyWindow, 1},
		{"an exit a nanosecond younger stays", unhealthyWindow - time.Nanosecond, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, clock := newClockedSupervisor()
			s.recordExit(exitInfo{code: 1})
			clock.Advance(tc.age)
			if got := s.recordExit(exitInfo{code: 1}); got != tc.want {
				t.Fatalf("the window holds %d exits, want %d", got, tc.want)
			}
		})
	}
}

func TestRecordExitMarksUnhealthyBeforeTheFifthEventIsSent(t *testing.T) {
	s, _ := newClockedSupervisor()
	seen := make(chan bool, 1)
	go func() {
		for i := 0; i < unhealthyExits; i++ {
			<-s.Exits()
		}
		seen <- s.Unhealthy()
	}()
	for i := 0; i < unhealthyExits; i++ {
		s.recordExit(exitInfo{code: 1})
	}
	select {
	case unhealthy := <-seen:
		if !unhealthy {
			t.Fatal("the fifth event arrived before the unhealthy flag was set")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the events never arrived")
	}
}

func TestRecordExitCarriesTheSpawnError(t *testing.T) {
	s, _ := newClockedSupervisor()
	boom := errors.New("cannot spawn")
	s.recordExit(exitInfo{code: -1, spawnErr: boom})
	if e := <-s.Exits(); e.Code != -1 || !errors.Is(e.Err, boom) || e.At.IsZero() {
		t.Fatalf("event %+v", e)
	}
}

func TestRecordExitDropsEventsWhenNobodyReads(t *testing.T) {
	s, _ := newClockedSupervisor()
	for i := 0; i < cap(s.events)+4; i++ {
		s.recordExit(exitInfo{code: i}) // must not block once the buffer is full
	}
	if got := len(s.events); got != cap(s.events) {
		t.Fatalf("%d events are buffered, want a full buffer of %d", got, cap(s.events))
	}
}

func TestSupervisorAwaitHealthyRefusesWhileStopping(t *testing.T) {
	probed := false
	s := NewSupervisor(SupervisorOptions{Health: func(context.Context) error {
		probed = true
		return nil
	}})
	s.stopping = true
	err := s.awaitHealthy(context.Background(), &child{})
	if !errors.Is(err, errStopping) {
		t.Fatalf("want the stopping error, got %v", err)
	}
	if probed {
		t.Fatal("a stopping supervisor must not probe a new child")
	}
}

func TestSupervisorAwaitHealthyStopsWhenTheChildExits(t *testing.T) {
	s := NewSupervisor(SupervisorOptions{Health: func(context.Context) error { return errors.New("not yet") }})
	c := &child{err: errors.New("killed")}
	c.exited.Store(true)
	err := s.awaitHealthy(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "exited with code -1 before becoming healthy") {
		t.Fatalf("got %v", err)
	}
}

func TestSupervisorAwaitHealthyStopsWhenTheContextEnds(t *testing.T) {
	s := NewSupervisor(SupervisorOptions{Health: func(context.Context) error { return errors.New("not yet") }})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.awaitHealthy(ctx, &child{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want the cancellation", err)
	}
}

func TestSupervisorAwaitHealthyTimesOut(t *testing.T) {
	s := NewSupervisor(SupervisorOptions{
		Health:       func(context.Context) error { return errors.New("not yet") },
		StartTimeout: time.Millisecond,
	})
	err := s.awaitHealthy(context.Background(), &child{})
	if err == nil || !strings.Contains(err.Error(), "did not become healthy within 1ms") {
		t.Fatalf("got %v", err)
	}
}

func TestExitCode(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Errorf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(errors.New("not an exit status")); got != -1 {
		t.Errorf("exitCode(other error) = %d, want -1", got)
	}
	if got := exitCode(exec.Command("sh", "-c", "exit 7").Run()); got != 7 {
		t.Errorf("exitCode(exit 7) = %d, want 7", got)
	}
	if got := exitCode(exec.ErrWaitDelay); got != 0 {
		t.Errorf("exitCode(ErrWaitDelay) = %d, want 0: Wait says so only for a child that exited cleanly", got)
	}
}

func TestNewSupervisorDefaults(t *testing.T) {
	s := NewSupervisor(SupervisorOptions{})
	if s.opts.StartTimeout != 60*time.Second || s.opts.Grace != 10*time.Second {
		t.Fatalf("start timeout %v and grace %v, want 60s and 10s", s.opts.StartTimeout, s.opts.Grace)
	}
	if s.opts.Logger == nil || s.opts.Now == nil || s.opts.Sleep == nil {
		t.Fatal("the logger, the clock and the sleep must have defaults")
	}
	s.opts.Logger.Info("discarded") // the default logger swallows this
	if skew := time.Since(s.opts.Now()); skew < -time.Second || skew > time.Second {
		t.Fatalf("the default clock is %v off", skew)
	}
	if err := s.opts.Sleep(context.Background(), 5*time.Millisecond); err != nil {
		t.Fatalf("the default sleep failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.opts.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("the default sleep ended with %v, want the cancellation", err)
	}
}

// A child that ends before it answers leaves the reason as its last line, such as a library that the loader cannot
// find; the error carries it, so the daemon's log and the service's journal name the cause.
func TestSupervisorStartNamesWhatTheChildLastLogged(t *testing.T) {
	const reason = "llama-server: error while loading shared libraries: libgomp.so.1: cannot open shared object file"
	// Under -cover the helper is a coverage binary, which without GOCOVERDIR says so on its way out.
	s, _, logPath := helperSupervisor(t, "HELPER_FAIL_START="+reason, "GOCOVERDIR="+t.TempDir())
	if err := os.WriteFile(logPath, []byte("a line from an earlier run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := s.Start(context.Background())
	if err == nil || !strings.HasSuffix(err.Error(), "exited with code 127 before becoming healthy: "+reason) {
		t.Fatalf("got %v", err)
	}
}

// Nothing that an earlier run logged is taken for the reason of this one.
func TestSupervisorAwaitHealthyLeavesOutAnEarlierRunsLines(t *testing.T) {
	earlier := "a line from an earlier run\n"
	logPath := filepath.Join(t.TempDir(), "llama-server.log")
	if err := os.WriteFile(logPath, []byte(earlier), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSupervisor(SupervisorOptions{LogPath: logPath, Health: func(context.Context) error { return errors.New("not yet") }})
	c := &child{err: errors.New("killed"), logStart: int64(len(earlier))}
	c.exited.Store(true)
	err := s.awaitHealthy(context.Background(), c)
	if err == nil || !strings.HasSuffix(err.Error(), "exited with code -1 before becoming healthy") {
		t.Fatalf("got %v", err)
	}
}

// A log that rotated since the child started is read from its beginning, and blank lines at its end are passed over.
func TestSupervisorAwaitHealthyReadsARotatedLogFromItsStart(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "llama-server.log")
	if err := os.WriteFile(logPath, []byte("the reason\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSupervisor(SupervisorOptions{LogPath: logPath, Health: func(context.Context) error { return errors.New("not yet") }})
	c := &child{err: errors.New("killed"), logStart: 1 << 20}
	c.exited.Store(true)
	err := s.awaitHealthy(context.Background(), c)
	if err == nil || !strings.HasSuffix(err.Error(), "before becoming healthy: the reason") {
		t.Fatalf("got %v", err)
	}
}

// Only the end of a long run's output is read, and the reason is still its last line.
func TestSupervisorAwaitHealthyReadsTheEndOfALongRun(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "llama-server.log")
	long := strings.Repeat("a line of the server's output\n", 1000) + "the reason\n"
	if err := os.WriteFile(logPath, []byte(long), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSupervisor(SupervisorOptions{LogPath: logPath, Health: func(context.Context) error { return errors.New("not yet") }})
	c := &child{err: errors.New("killed")}
	c.exited.Store(true)
	err := s.awaitHealthy(context.Background(), c)
	if err == nil || !strings.HasSuffix(err.Error(), "before becoming healthy: the reason") {
		t.Fatalf("got %v", err)
	}
}
