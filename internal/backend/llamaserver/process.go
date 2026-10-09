package llamaserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// ExitEvent is one unexpected child exit, or one restart that could not start a child at all.
type ExitEvent struct {
	Code int // the child's exit code; -1 when a signal ended it or no child could be started
	At   time.Time
	Err  error // why no child could be started; nil when a child ran and exited
}

// SupervisorOptions describe the child and the probes around it. Binary, LogPath and Health are required.
type SupervisorOptions struct {
	Binary  string
	Args    []string
	Env     []string // the child's whole environment; nothing of the daemon's own is inherited
	LogPath string   // child stdout and stderr, rotated at 20 MB, three rotated files kept besides the active one
	// PIDFile, when set, holds the pid of the running child (mode 0600). A Start ends the llama-server
	// it names, with its process group, when an earlier daemon left it running; a clean Stop removes it.
	PIDFile      string
	Logger       *slog.Logger
	Health       func(ctx context.Context) error
	Sleep        func(ctx context.Context, d time.Duration) error
	Now          func() time.Time
	StartTimeout time.Duration // how long a start waits for the first healthy answer, 60 seconds by default
	Grace        time.Duration // how long Stop waits between SIGTERM and SIGKILL, ten seconds by default
}

const (
	defaultGrace    = 10 * time.Second
	unhealthyWindow = 10 * time.Minute
	unhealthyExits  = 5
	// defaultWaitDelay bounds how long Wait waits for the child's output to close after the child has
	// exited: a model process the router started may hold the log pipe for as long as it lives.
	defaultWaitDelay = 5 * time.Second
)

var (
	// errStopping is what a start or a health wait returns once Stop has been called.
	errStopping = errors.New("llamaserver: the supervisor is stopping")
	// errAlreadyStarted is what Start returns while another Start is in flight or after one succeeded.
	errAlreadyStarted = errors.New("llamaserver: the supervisor is already started")
)

// Backoff is the wait before restart number n (0-based): 1, 2, 4, 8, 16, then 30 seconds.
// A negative n counts as 0.
func Backoff(n int) time.Duration {
	if n >= 5 {
		return 30 * time.Second
	}
	return time.Duration(1<<max(n, 0)) * time.Second
}

// child is one spawned process. exited flips when its Wait returns; the exit is then queued for watch.
type child struct {
	cmd    *exec.Cmd
	exited atomic.Bool
	err    error
}

// exitInfo is one end of a run: a child that exited, with its pid, or a restart that could not start one.
type exitInfo struct {
	code     int
	pid      int
	spawnErr error
}

// Supervisor keeps one child process running.
type Supervisor struct {
	opts       SupervisorOptions
	ctx        context.Context // ends when Stop is called; the watcher's sleeps and probes run under it
	cancel     context.CancelFunc
	newLog     func(path string) io.WriteCloser        // the rotating log; tests swap it
	sendSignal func(pid int, sig syscall.Signal) error // signalGroup; tests swap it
	waitDelay  time.Duration                           // the child's WaitDelay; tests shorten it

	mu        sync.Mutex
	cur       *child
	stopping  bool
	unhealthy bool
	starting  chan struct{} // open while a Start is in flight
	done      chan struct{} // closed when the watcher has finished
	exits     []time.Time
	events    chan ExitEvent
	exited    chan *child
	log       io.WriteCloser
}

// NewSupervisor fills the defaults: a real sleep and clock, a 60-second start timeout, a ten-second
// stop grace, a discard logger.
func NewSupervisor(opts SupervisorOptions) *Supervisor {
	if opts.Sleep == nil {
		opts.Sleep = func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.StartTimeout == 0 {
		opts.StartTimeout = 60 * time.Second
	}
	if opts.Grace == 0 {
		opts.Grace = defaultGrace
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Supervisor{
		opts:       opts,
		ctx:        ctx,
		cancel:     cancel,
		newLog:     openLog,
		sendSignal: signalGroup,
		waitDelay:  defaultWaitDelay,
		events:     make(chan ExitEvent, 16),
		exited:     make(chan *child, 1),
	}
}

// openLog is the rotating log a child writes to: 20 MB per file, three rotated files kept besides the
// active one.
func openLog(path string) io.WriteCloser {
	return &lumberjack.Logger{Filename: path, MaxSize: 20, MaxBackups: 3}
}

// validate refuses options a supervisor cannot run without.
func (o SupervisorOptions) validate() error {
	switch {
	case o.Binary == "":
		return errors.New("llamaserver: the supervisor needs a Binary")
	case o.LogPath == "":
		return errors.New("llamaserver: the supervisor needs a LogPath")
	case o.Health == nil:
		return errors.New("llamaserver: the supervisor needs a Health probe")
	}
	return nil
}

// Exits delivers every unexpected exit and every restart that could not start a child; the channel is
// buffered and never closed.
func (s *Supervisor) Exits() <-chan ExitEvent { return s.events }

// PID is the child's process id, 0 when nothing runs.
func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return 0
	}
	return s.cur.cmd.Process.Pid
}

// Running reports whether a child is alive.
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != nil && !s.cur.exited.Load()
}

// Unhealthy reports five exits inside ten minutes; restarts have stopped.
func (s *Supervisor) Unhealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unhealthy
}

// Start spawns the child, waits for it to become healthy and begins supervising it. A child that
// exits, or stays unhealthy past StartTimeout, is killed and the error returned. A Supervisor serves
// one Start and one Stop: Start fails once a Start is in flight or has succeeded, and once Stop has
// been called. A Stop that lands while Start waits makes Start kill its child and fail.
func (s *Supervisor) Start(ctx context.Context) error {
	if err := s.opts.validate(); err != nil {
		return err
	}
	finish, err := s.beginStart()
	if err != nil {
		return err
	}
	defer finish()
	if err := os.MkdirAll(filepath.Dir(s.opts.LogPath), 0o750); err != nil {
		return fmt.Errorf("llamaserver: create the log directory: %w", err)
	}
	s.log = s.newLog(s.opts.LogPath)
	if err := s.stopLeftover(ctx); err != nil {
		s.closeLog()
		return err
	}
	c, err := s.spawn()
	if err != nil {
		s.closeLog()
		return err
	}
	if err := s.awaitHealthy(ctx, c); err != nil {
		s.abandon(c)
		return err
	}
	done, err := s.publish()
	if err != nil {
		s.abandon(c)
		return err
	}
	go s.watch(s.ctx, done)
	return nil
}

// beginStart claims the one Start slot. It refuses once Stop has been called, before anything is forked,
// and returns the function that gives Stop its signal that this Start is over.
func (s *Supervisor) beginStart() (finish func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.stopping:
		return nil, errStopping
	case s.starting != nil || s.done != nil:
		return nil, errAlreadyStarted
	}
	starting := make(chan struct{})
	s.starting = starting
	return func() {
		s.mu.Lock()
		s.starting = nil
		s.mu.Unlock()
		close(starting)
	}, nil
}

// publish opens the watcher's done channel, unless a Stop landed while the child was starting.
func (s *Supervisor) publish() (chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return nil, errStopping
	}
	s.done = make(chan struct{})
	return s.done, nil
}

// abandon kills a child that will not be supervised, waits for its exit and releases the log and the
// pid file.
func (s *Supervisor) abandon(c *child) {
	s.kill(c)
	<-s.exited // the wait goroutine reports the killed child; drain it
	s.mu.Lock()
	s.cur = nil
	s.mu.Unlock()
	s.closeLog()
	s.removePIDFile()
}

// spawn starts the binary and a goroutine that waits for it and queues its exit, and records the pid.
func (s *Supervisor) spawn() (*child, error) {
	cmd := exec.Command(s.opts.Binary, s.opts.Args...) //nolint:gosec // the binary is the verified runtime
	// Env is never nil: a nil Env would hand the child the daemon's own environment.
	cmd.Env = append([]string{}, s.opts.Env...)
	cmd.Stdout, cmd.Stderr = s.log, s.log
	cmd.WaitDelay = s.waitDelay // a model process that keeps the log pipe open cannot hold Wait
	ownProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("llamaserver: start %s: %w", s.opts.Binary, err)
	}
	c := &child{cmd: cmd}
	s.mu.Lock()
	s.cur = c
	s.mu.Unlock()
	s.opts.Logger.Info("llama-server started", "pid", cmd.Process.Pid)
	s.writePIDFile(cmd.Process.Pid)
	go func() {
		c.err = cmd.Wait()
		c.exited.Store(true)
		s.exited <- c
	}()
	return c, nil
}

// awaitHealthy polls the health probe until it answers, the child exits, the context ends, or the
// start timeout passes. It never consumes the child's exit; watch does. Once Stop has been called it
// gives up at once, so a child spawned as the stop landed is never accepted.
func (s *Supervisor) awaitHealthy(ctx context.Context, c *child) error {
	deadline := time.Now().Add(s.opts.StartTimeout)
	for {
		if s.isStopping() {
			return errStopping
		}
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := s.opts.Health(probe)
		cancel()
		if err == nil {
			return nil
		}
		switch {
		case c.exited.Load():
			return fmt.Errorf("llamaserver: the server exited with code %d before becoming healthy", exitCode(c.err))
		case ctx.Err() != nil:
			return ctx.Err()
		case time.Now().After(deadline):
			return fmt.Errorf("llamaserver: the server did not become healthy within %s", s.opts.StartTimeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// watch consumes exits, restarts with backoff, and gives up after five exits inside ten minutes. A
// restart that cannot spawn counts as an exit and is retried the same way. When watch ends no child
// is left to write to the log, so it closes the log and then releases Stop through done.
func (s *Supervisor) watch(ctx context.Context, done chan struct{}) {
	defer func() {
		s.closeLog()
		close(done)
	}()
	var failed *exitInfo // set when the last restart could not spawn, so no child exit is coming
	for {
		var info exitInfo
		if failed != nil {
			info, failed = *failed, nil
		} else {
			info = s.waitExit()
		}
		if s.isStopping() {
			return
		}
		if info.spawnErr == nil {
			s.killGroup(info.pid)
			s.opts.Logger.Warn("llama-server exited", "code", info.code)
		}
		n := s.recordExit(info)
		if n >= unhealthyExits {
			s.opts.Logger.Error("llama-server exited five times in ten minutes; giving up until taraengine restarts")
			return
		}
		if err := s.opts.Sleep(ctx, Backoff(n-1)); err != nil {
			return
		}
		if s.isStopping() {
			return // a stop that raced the end of the backoff wins; no child may start after it
		}
		next, err := s.spawn()
		if err != nil {
			s.opts.Logger.Error("llama-server restart failed", "error", err)
			failed = &exitInfo{code: -1, spawnErr: err}
			continue
		}
		if err := s.awaitHealthy(ctx, next); err != nil {
			s.opts.Logger.Error("llama-server restart unhealthy", "error", err)
			s.kill(next) // its exit arrives on the channel and counts as the next one
		}
	}
}

// waitExit blocks until the running child has exited and forgets it.
func (s *Supervisor) waitExit() exitInfo {
	c := <-s.exited
	s.mu.Lock()
	if s.cur == c {
		s.cur = nil
	}
	s.mu.Unlock()
	return exitInfo{code: exitCode(c.err), pid: c.cmd.Process.Pid}
}

// recordExit stores the exit, drops exits older than the window, marks the supervisor unhealthy when
// the window holds five, emits the event, and returns how many exits the window holds. The flag is set
// before the event is sent, so whoever reads the event sees the verdict.
func (s *Supervisor) recordExit(info exitInfo) int {
	now := s.opts.Now()
	s.mu.Lock()
	kept := s.exits[:0:0]
	for _, at := range s.exits {
		if now.Sub(at) < unhealthyWindow {
			kept = append(kept, at)
		}
	}
	s.exits = append(kept, now)
	n := len(s.exits)
	if n >= unhealthyExits {
		s.unhealthy = true
	}
	s.mu.Unlock()
	select {
	case s.events <- ExitEvent{Code: info.code, At: now, Err: info.spawnErr}:
	default:
	}
	return n
}

// Stop ends supervision and returns once the watcher has finished. A Start in flight fails with the
// stopping error and kills its child before Stop goes on. A pending restart is cancelled. A running
// child gets SIGTERM to its process group, then SIGKILL after Grace. Once the watcher is done the group
// gets SIGKILL, for a model process that ignored SIGTERM and kept the log pipe open past the wait delay,
// and the pid file goes. If ctx ends first, Stop kills the child and returns ctx.Err(); the watcher still
// finishes in the background and closes the log, and the pid file stays for the next start to check.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	starting := s.starting
	s.mu.Unlock()
	s.cancel()
	if starting != nil {
		select {
		case <-starting:
		case <-ctx.Done():
			s.kill(s.current())
			return ctx.Err()
		}
	}
	s.mu.Lock()
	c, done := s.cur, s.done
	s.mu.Unlock()
	if done == nil {
		return nil // Start never succeeded, so there is no watcher to wait for
	}
	s.signal(c, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(s.opts.Grace):
		s.kill(c)
		<-done
	case <-ctx.Done():
		s.kill(c)
		return ctx.Err()
	}
	if c != nil {
		s.killGroup(c.cmd.Process.Pid)
	}
	s.removePIDFile()
	return nil
}

// signal sends sig to the child's process group. An exited child is left alone, and an error other
// than "no such process" is logged.
func (s *Supervisor) signal(c *child, sig syscall.Signal) {
	if c == nil || c.exited.Load() {
		return
	}
	pid := c.cmd.Process.Pid
	if err := s.sendSignal(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		s.opts.Logger.Warn("llama-server signal failed", "signal", sig.String(), "pid", pid, "error", err)
	}
}

// kill ends the child's process group at once.
func (s *Supervisor) kill(c *child) { s.signal(c, syscall.SIGKILL) }

func (s *Supervisor) current() *child {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *Supervisor) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// closeLog releases the rotating log once no child can write to it any more.
func (s *Supervisor) closeLog() {
	if s.log != nil {
		_ = s.log.Close()
	}
}

// exitCode is the code a child's Wait reports. ErrWaitDelay comes only for a child that exited cleanly
// while something it started still held its output, so it is the code 0.
func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return 0
	}
	return -1
}
