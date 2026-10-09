//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/measure"
	"github.com/tara-vision/taraengine/pkg/client"
)

const (
	// bootTimeout bounds the time from exec to a healthy daemon. The first start of a freshly unpacked runtime is
	// slow on macOS, where the system checks the new binaries before it runs them.
	bootTimeout = 90 * time.Second
	// stopTimeout bounds the time from SIGTERM to exit. The serve contract is a few seconds at most.
	stopTimeout = 20 * time.Second
	// cliTimeout bounds one run of the binary as a client of the daemon.
	cliTimeout = 30 * time.Second
	// ownedEnvPrefix marks the variables of taraengine itself; the daemon and the client commands run without them,
	// so that a key or a token in the environment of whoever runs the test cannot change what is tested.
	ownedEnvPrefix = "TARAENGINE_"
)

var (
	listeningLine = regexp.MustCompile(`msg="taraengine listening" listen=(\S+)`)
	errorLogLine  = regexp.MustCompile(`(?m)^.*\blevel=ERROR\b.*$`)
)

// syncBuffer collects the output of the daemon, which os/exec copies on a goroutine of its own.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// shutdown is how a stop went: the error, if any, whether the daemon had already exited when the stop began, and the
// processes that lived below the daemon at that moment.
type shutdown struct {
	err         error
	alreadyGone bool
	tree        []int
}

// daemon is one `taraengine serve` process.
type daemon struct {
	bin        string
	dataDir    string
	configPath string
	cmd        *exec.Cmd
	logs       *syncBuffer
	done       chan struct{} // closed once the process has been waited for
	waitErr    error         // the result of the wait, valid once done is closed
	baseURL    string        // where the daemon says it listens, set during start

	stopOnce sync.Once
	stopped  atomic.Bool
	result   shutdown
}

// startDaemon runs `taraengine serve` on an address the system picks, waits until it answers, and arranges for it to
// be stopped when the test ends, whatever way that happens.
func startDaemon(t *testing.T, env environment, dataDir, configPath string) *daemon {
	t.Helper()
	d := &daemon{
		bin: env.bin, dataDir: dataDir, configPath: configPath, logs: &syncBuffer{}, done: make(chan struct{}),
	}
	d.cmd = exec.Command(env.bin, "serve", "--config", configPath)
	d.cmd.Env = environmentWithout(ownedEnvPrefix)
	d.cmd.Stdout, d.cmd.Stderr = d.logs, d.logs
	d.cmd.WaitDelay = 5 * time.Second // a stray descendant that holds the output open cannot hold the wait forever
	if err := d.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", env.bin, err)
	}
	go func() {
		d.waitErr = d.cmd.Wait()
		close(d.done)
	}()
	// Cleanups run last to first: the daemon is stopped, then its output is shown if the test failed.
	t.Cleanup(func() { d.dumpOnFailure(t) })
	t.Cleanup(func() { d.cleanup(t) })
	d.awaitHealthy(t)
	return d
}

// environmentWithout is the environment of this process minus the variables that start with prefix.
func environmentWithout(prefix string) []string {
	var kept []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, prefix) {
			kept = append(kept, kv)
		}
	}
	return kept
}

// listenAddress is the address the daemon logged when it began to listen, or "" before that.
func listenAddress(logs string) string {
	if m := listeningLine.FindStringSubmatch(logs); m != nil {
		return m[1]
	}
	return ""
}

// awaitHealthy waits for the listening line, which gives the address the system picked, and then for the health
// route to answer ok. A daemon that exits on the way ends the wait at once, with its output.
func (d *daemon) awaitHealthy(t *testing.T) {
	t.Helper()
	deadline := time.After(bootTimeout)
	pause := func(what string) {
		select {
		case <-d.done:
			t.Fatalf("serve exited before it was %s: %v\n%s", what, d.waitErr, d.logs)
		case <-deadline:
			t.Fatalf("serve was not %s within %s\n%s", what, bootTimeout, d.logs)
		case <-time.After(100 * time.Millisecond):
		}
	}
	for d.baseURL == "" {
		if addr := listenAddress(d.logs.String()); addr != "" {
			d.baseURL = "http://" + addr
			break
		}
		pause("listening")
	}
	c := client.New(d.baseURL, "")
	for {
		ctx, cancel := context.WithTimeout(t.Context(), callTimeout)
		health, err := c.Health(ctx)
		cancel()
		if err == nil && health.Status == "ok" {
			return
		}
		pause("healthy")
	}
}

// cleanup stops a daemon that no step has stopped, and fails the test when that does not go cleanly. A daemon that
// died on its own is a failure of its own, unless the test has failed already and so has said what was wrong. The
// runtime of such a daemon may have outlived it, and is swept away: no test leaves a runtime behind.
func (d *daemon) cleanup(t *testing.T) {
	if d.stopped.Load() {
		return // a step stopped the daemon and judged how it went
	}
	sd := d.stop()
	if sd.err != nil && (!sd.alreadyGone || !t.Failed()) {
		t.Errorf("stopping the daemon at the end of the test: %v", sd.err)
	}
	if sd.alreadyGone {
		d.sweep(t)
	}
}

// sweep ends what is left of the runtime of a daemon that died on its own. The processes get SIGTERM, as a service
// manager would send it, and awaitGone waits for them and kills the ones that stay.
func (d *daemon) sweep(t *testing.T) {
	t.Helper()
	strays := d.strays()
	for _, pid := range strays {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	awaitGone(t, strays)
}

// stop sends SIGTERM, as a service manager does, and waits for the daemon to exit. A daemon that is still there
// after stopTimeout is killed together with the processes below it, so that no test leaves a runtime behind. The
// first call does the work and every later call returns its result.
func (d *daemon) stop() shutdown {
	d.stopOnce.Do(func() {
		d.stopped.Store(true)
		d.result = d.terminate()
	})
	return d.result
}

func (d *daemon) terminate() shutdown {
	select {
	case <-d.done:
		err := fmt.Errorf("serve had already exited before it was stopped (%v)", d.waitErr)
		return shutdown{err: err, alreadyGone: true}
	default:
	}
	tree := d.below()
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return shutdown{err: fmt.Errorf("signal serve: %w", err), tree: tree}
	}
	select {
	case <-d.done:
		return shutdown{err: d.waitErr, tree: tree}
	case <-time.After(stopTimeout):
		d.killAll(tree)
		<-d.done
		return shutdown{err: fmt.Errorf("serve did not stop within %s of SIGTERM", stopTimeout), tree: tree}
	}
}

// below lists the processes that descend from the daemon, from the process table of the system. A table that cannot
// be read gives an empty list, which the steps that depend on the list refuse.
func (d *daemon) below() []int {
	table, err := exec.Command("ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	var tree []int
	for _, pid := range measure.Descendants(string(table), d.cmd.Process.Pid) {
		if pid != d.cmd.Process.Pid {
			tree = append(tree, pid)
		}
	}
	return tree
}

// strays lists the processes of the system, other than this test, whose command line names the data directory of
// the daemon. The router of the runtime and its model processes are such processes, and after a daemon has died they
// no longer descend from it, so the process table cannot find them by their parents. A table that cannot be read
// gives an empty list.
func (d *daemon) strays() []int {
	table, err := exec.Command("ps", "-e", "-ww", "-o", "pid=", "-o", "command=").Output()
	if err != nil {
		return nil
	}
	return pidsMentioning(string(table), d.dataDir, os.Getpid())
}

// pidsMentioning reads the output of `ps -o pid= -o command=` and lists the processes whose command line contains
// marker, leaving out the process ids in except. The lines that do not start with a process id are ignored, and an
// empty marker names nothing: it would otherwise match every process of the system.
func pidsMentioning(table, marker string, except ...int) []int {
	if marker == "" {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(table, "\n") {
		text, command, found := strings.Cut(strings.TrimSpace(line), " ")
		pid, err := strconv.Atoi(text)
		if !found || err != nil || !strings.Contains(command, marker) || slices.Contains(except, pid) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// killAll ends the daemon and the listed processes at once. It is the last resort of a stop that failed.
func (d *daemon) killAll(tree []int) {
	_ = d.cmd.Process.Kill()
	for _, pid := range tree {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// dumpOnFailure shows what the daemon and its runtime wrote, once the daemon is down and only if the test failed.
func (d *daemon) dumpOnFailure(t *testing.T) {
	if !t.Failed() {
		return
	}
	t.Logf("taraengine serve output:\n%s", d.logs)
	if tail := tailOf(filepath.Join(d.dataDir, "logs", "llama-server.log"), 60); tail != "" {
		t.Logf("end of the llama-server log:\n%s", tail)
	}
}

// tailOf is the last n lines of the file, or "" when there is no such file.
func tailOf(path string, n int) string {
	data, err := os.ReadFile(path) //nolint:gosec // the log of the daemon under test
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	return strings.Join(lines[max(len(lines)-n, 0):], "\n")
}

// errorLines are the lines of the daemon's output that were logged at the error level.
func (d *daemon) errorLines() []string {
	return errorLogLine.FindAllString(d.logs.String(), -1)
}

// cliResult is what one run of the binary left behind.
type cliResult struct {
	stdout, stderr string
	code           int
}

// cli runs the built binary as the client of this daemon, with the daemon's address and configuration. A nonzero
// exit is a result; only a binary that cannot be run at all fails the test.
func (d *daemon) cli(t *testing.T, args ...string) cliResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.bin, append(slices.Clone(args), "--host", d.baseURL, "--config", d.configPath)...)
	cmd.Env = environmentWithout(ownedEnvPrefix)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := cliResult{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		res.code = exit.ExitCode()
	default:
		t.Fatalf("run %s %v: %v", d.bin, args, err)
	}
	return res
}
