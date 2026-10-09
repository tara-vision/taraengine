package llamaserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

func (c *fakeClock) Sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// Advance moves the clock without recording a sleep.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// helperSupervisor is a supervisor whose child is the test binary playing llama-server. Its cleanup stops
// the supervisor, so a test that fails after Start never leaves the helper running.
func helperSupervisor(t *testing.T, env ...string) (*Supervisor, *fakeClock, string) {
	t.Helper()
	port := freePort(t)
	clock := &fakeClock{now: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	logPath := filepath.Join(t.TempDir(), "llama-server.log")
	client := NewClient("http://127.0.0.1:"+strconv.Itoa(port), "")
	s := NewSupervisor(SupervisorOptions{
		Binary:       os.Args[0],
		Args:         []string{"--port", strconv.Itoa(port)},
		Env:          append([]string{"TARAENGINE_HELPER=1"}, env...),
		LogPath:      logPath,
		Health:       client.Health,
		Sleep:        clock.Sleep,
		Now:          clock.Now,
		StartTimeout: 5 * time.Second,
	})
	t.Cleanup(func() { stopAndWait(t, s) })
	return s, clock, logPath
}

// stopAndWait stops the supervisor and fails the test unless the stop saw its child exit. A Stop that
// returns nil has waited for the watcher, and the watcher only ends once the last child's exit is in.
func stopAndWait(t *testing.T, s *Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Errorf("the supervisor did not stop: %v", err)
	}
}

// helperPort is the port the helper child of s listens on.
func helperPort(s *Supervisor) string { return s.opts.Args[1] }

// helperAlive reports whether a helper process started with --port <port> is still running.
func helperAlive(t *testing.T, port string) bool {
	t.Helper()
	out, err := exec.Command("ps", "-axww", "-o", "command").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), " --port "+port) {
			return true
		}
	}
	return false
}

// logSpy stands in for the rotating log: it discards what the child writes and counts closes.
type logSpy struct {
	mu     sync.Mutex
	closed int
}

func (l *logSpy) Write(p []byte) (int, error) { return len(p), nil }

func (l *logSpy) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed++
	return nil
}

func (l *logSpy) Closes() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// logRecorder keeps what the supervisor logs, so a test can count the children it started.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	line := rec.Message
	rec.Attrs(func(a slog.Attr) bool {
		line += fmt.Sprintf(" %s=%v", a.Key, a.Value)
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *logRecorder) WithGroup(string) slog.Handler { return r }

// Count is how many logged lines start with prefix.
func (r *logRecorder) Count(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, line := range r.lines {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// startedPIDs are the pids of the children the supervisor logged as started.
func (r *logRecorder) startedPIDs() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var pids []int
	for _, line := range r.lines {
		var pid int
		if _, err := fmt.Sscanf(line, "llama-server started pid=%d", &pid); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// Contains reports whether any logged line holds text.
func (r *logRecorder) Contains(text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range r.lines {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

// parkedSleep records the sleep on the fake clock, tells the test the watcher is now in its backoff,
// then waits for the context to end, the way the real sleep does, and reports how it ended.
func parkedSleep(clock *fakeClock, parked chan<- struct{}, ended chan<- error) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		if err := clock.Sleep(ctx, d); err != nil {
			return err
		}
		parked <- struct{}{}
		<-ctx.Done()
		ended <- ctx.Err()
		return ctx.Err()
	}
}

// nextEvents waits for n exit events.
func nextEvents(t *testing.T, s *Supervisor, n int) []ExitEvent {
	t.Helper()
	events := make([]ExitEvent, 0, n)
	for len(events) < n {
		select {
		case e := <-s.Exits():
			events = append(events, e)
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d exit events seen", len(events), n)
		}
	}
	return events
}

// eventually polls cond until it holds or the time is up.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitStopping waits until a Stop has registered itself.
func waitStopping(t *testing.T, s *Supervisor) {
	t.Helper()
	eventually(t, 2*time.Second, "the stop to register", s.isStopping)
}

// recvErr waits for a result from a goroutine.
func recvErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
		return nil
	}
}

// linkToTestBinary returns a path that runs the test binary, and that path's target. A test can remove
// the link to make every later spawn fail, and put it back to make spawns work again.
func linkToTestBinary(t *testing.T) (link, target string) {
	t.Helper()
	target, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(t.TempDir(), "llama-server")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link, target
}

func TestSupervisorStartsAndStops(t *testing.T) {
	s, _, logPath := helperSupervisor(t)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !s.Running() || s.PID() <= 0 {
		t.Fatal("not running after start")
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if s.Running() {
		t.Fatal("running after stop")
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "helper started") {
		t.Fatalf("child output not logged: %q", data)
	}
	select {
	case e := <-s.Exits():
		t.Fatalf("a stop must not count as an exit: %+v", e)
	default:
	}
}

func TestSupervisorRestartsWithBackoff(t *testing.T) {
	s, clock, _ := helperSupervisor(t, "HELPER_EXIT_AFTER_MS=300", "HELPER_EXIT_CODE=3")
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	var codes []int
	for len(codes) < 3 {
		select {
		case e := <-s.Exits():
			codes = append(codes, e.Code)
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d exits seen", len(codes))
		}
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if s.Running() {
		t.Fatal("running after stop")
	}
	time.Sleep(200 * time.Millisecond)
	if helperAlive(t, helperPort(s)) {
		t.Fatal("a helper process is still alive after stop")
	}
	if codes[0] != 3 || codes[1] != 3 || codes[2] != 3 {
		t.Fatalf("exit codes %v", codes)
	}
	sleeps := clock.Sleeps()
	if len(sleeps) < 2 || sleeps[0] != time.Second || sleeps[1] != 2*time.Second {
		t.Fatalf("backoff sleeps %v", sleeps)
	}
}

func TestSupervisorGivesUpAfterFiveExits(t *testing.T) {
	s, _, _ := helperSupervisor(t, "HELPER_EXIT_AFTER_MS=300", "HELPER_EXIT_CODE=1")
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	nextEvents(t, s, 5)
	// the flag is set before the fifth event is sent, so a reader of the event can rely on it
	if !s.Unhealthy() || s.Running() {
		t.Fatalf("after five exits: unhealthy=%v running=%v", s.Unhealthy(), s.Running())
	}
	_ = s.Stop(ctx)
}

func TestSupervisorStartTimeout(t *testing.T) {
	s, _, _ := helperSupervisor(t, "HELPER_NO_HEALTH=1")
	s.opts.StartTimeout = 300 * time.Millisecond
	err := s.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not become healthy") {
		t.Fatalf("want a start timeout, got %v", err)
	}
	if s.Running() {
		t.Fatal("the child must be killed after a start timeout")
	}
}

func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, d := range want {
		if got := Backoff(i); got != d {
			t.Errorf("Backoff(%d) = %v, want %v", i, got, d)
		}
	}
}

func TestBackoffNegativeIndex(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Backoff(-1) panicked: %v", r)
		}
	}()
	if got := Backoff(-1); got != time.Second {
		t.Fatalf("Backoff(-1) = %v, want %v", got, time.Second)
	}
}

func TestSupervisorStopDuringBackoffLeavesNoChild(t *testing.T) {
	s, clock, _ := helperSupervisor(t, "HELPER_EXIT_AFTER_MS=300", "HELPER_EXIT_CODE=3")
	parked := make(chan struct{}, 1)
	ended := make(chan error, 1)
	s.opts.Sleep = parkedSleep(clock, parked, ended)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher never reached its backoff")
	}
	if s.Running() {
		t.Fatal("a child is running during the backoff")
	}

	began := time.Now()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("stop took %v; cancelling the backoff should end it at once", took)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the backoff ended with %v, want a cancellation", err)
		}
	default:
		t.Fatal("stop returned while the watcher was still in its backoff")
	}
	select {
	case <-s.done:
	default:
		t.Fatal("stop returned before the watcher finished")
	}
	if s.Running() || s.PID() != 0 {
		t.Fatalf("after stop: running=%v pid=%d", s.Running(), s.PID())
	}
	time.Sleep(200 * time.Millisecond)
	if helperAlive(t, helperPort(s)) {
		t.Fatal("a helper process is still alive after stop")
	}
	if got := clock.Sleeps(); len(got) != 1 || got[0] != time.Second {
		t.Fatalf("backoff sleeps %v, want the single first backoff of 1s", got)
	}
}

// A stop that lands just as the backoff ends, when the sleep's timer wins the race against the
// cancellation, must not be followed by a restart: not a surviving child, and not even a spawn.
func TestSupervisorStopWhileBackoffEndsDoesNotRestart(t *testing.T) {
	s, clock, _ := helperSupervisor(t, "HELPER_EXIT_AFTER_MS=300", "HELPER_EXIT_CODE=3")
	rec := &logRecorder{}
	s.opts.Logger = slog.New(rec)
	parked := make(chan struct{}, 1)
	release := make(chan struct{})
	s.opts.Sleep = func(ctx context.Context, d time.Duration) error {
		if err := clock.Sleep(ctx, d); err != nil {
			return err
		}
		parked <- struct{}{}
		<-release // ignores the context: the sleep ends normally although a stop is pending
		return nil
	}
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher never reached its backoff")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- s.Stop(ctx) }()
	waitStopping(t, s)
	close(release)
	if err := recvErr(t, stopped, "stop"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if s.Running() || s.PID() != 0 {
		t.Fatalf("a restart followed the stop: running=%v pid=%d", s.Running(), s.PID())
	}
	if helperAlive(t, helperPort(s)) {
		t.Fatal("a helper process is alive after stop")
	}
	if got := rec.Count("llama-server started"); got != 1 {
		t.Fatalf("%d children were started, want only the first", got)
	}
}
