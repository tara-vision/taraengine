package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The sentences Install and Uninstall return.
const (
	noteInstalled = "installed and started the taraengine service"
	noteRestarted = "restarted the taraengine service"
	noteRelinked  = "the taraengine service restarts on the new binary"
	noteOldBinary = "the taraengine service runs %s until your next login and %s after it; " +
		"`taraengine service stop` and `taraengine service start` move it now"
	noteStopped    = "the taraengine service is stopped; `taraengine service start` starts it"
	noteNextLogin  = "installed the taraengine service; it starts at your next login, or with the first taraengine command"
	noteNextLoginE = ", for example `taraengine pull gemma4:12b`"
)

// The lines of `launchctl print` that State reads: they sit one tab in, where the nested sections sit deeper.
var (
	printState    = regexp.MustCompile(`(?m)^\tstate = (.+)$`)
	printPID      = regexp.MustCompile(`(?m)^\tpid = ([0-9]+)$`)
	printLastExit = regexp.MustCompile(`(?m)^\tlast exit code = (.+)$`)
)

// Launchd is the LaunchAgent of one user. Its paths come from the user's home in the user database: inside
// Homebrew's sandbox HOME points at a temporary directory, which Sandboxed records.
type Launchd struct {
	Run       Runner
	UID       int
	Home      string
	Sandboxed bool
}

var _ Manager = (*Launchd)(nil)

// currentUser is user.Current. It is a variable only so that a test can make the lookup fail.
var currentUser = user.Current

// goneWait bounds the wait for a job that bootout is still taking down, and gonePoll spaces the looks at it. The engine
// takes seconds to exit: the requests in flight, then llama-server. Variables so that a test can shorten them.
var (
	goneWait = 30 * time.Second
	gonePoll = 250 * time.Millisecond
)

// NewLaunchd is the agent of the user who runs this process.
func NewLaunchd(run Runner) (*Launchd, error) {
	u, err := currentUser()
	if err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, fmt.Errorf("service: user id %q: %w", u.Uid, err)
	}
	// Homebrew's sandbox gives its steps a HOME of their own; a HOME written another way is still the user's.
	sandboxed := filepath.Clean(os.Getenv("HOME")) != filepath.Clean(u.HomeDir)
	return &Launchd{Run: run, UID: uid, Home: u.HomeDir, Sandboxed: sandboxed}, nil
}

// PlistPath is where the agent's definition lives.
func (l *Launchd) PlistPath() string {
	return filepath.Join(l.Home, "Library", "LaunchAgents", Label+".plist")
}

// LogPath is where launchd writes what the daemon prints: its warnings and errors.
func (l *Launchd) LogPath() string {
	return filepath.Join(l.Home, "Library", "Logs", "taraengine", "service.log")
}

func (l *Launchd) domain() string { return "gui/" + strconv.Itoa(l.UID) }
func (l *Launchd) target() string { return l.domain() + "/" + Label }

func (l *Launchd) launchctl(ctx context.Context, args ...string) error {
	out, err := l.Run(ctx, "launchctl", args...)
	if err != nil {
		return fmt.Errorf("service: launchctl %s: %s (%w)", args[0], strings.TrimSpace(out), err)
	}
	return nil
}

// Install writes the plist for binary and makes the agent run it. A loaded agent is restarted in place, or loaded
// again when its definition changed; an agent that is not loaded is loaded. A stopped agent stays stopped. Inside
// Homebrew's sandbox launchd refuses to load an agent, and a kickstart there blocked an upgrade for minutes: a first
// install starts at the next login or with the first command, and an upgrade restarts nothing itself. The old cask's
// uninstall step stopped the agent after Homebrew unlinked its binary, Homebrew links the new binary before this step,
// and KeepAlive on that path brings the agent back on it. An agent that another binary's definition loaded keeps that
// binary until the definition is loaded again, and Install says so.
func (l *Launchd) Install(ctx context.Context, binary string) (string, error) {
	changed, err := l.writePlist(RenderPlist(binary, l.LogPath()))
	if err != nil {
		return "", err
	}
	disabled, err := l.disabled(ctx)
	if err != nil {
		return "", err
	}
	if disabled {
		return noteStopped, nil
	}
	program, loaded := l.loadedProgram(ctx)
	switch {
	case loaded && l.Sandboxed && program != "" && program != binary:
		return fmt.Sprintf(noteOldBinary, program, binary), nil
	case loaded && l.Sandboxed:
		return noteRelinked, nil
	case loaded && !changed:
		return noteRestarted, l.launchctl(ctx, "kickstart", "-k", l.target())
	case loaded:
		if err := l.bootout(ctx); err != nil {
			return "", err
		}
		return noteRestarted, l.launchctl(ctx, "bootstrap", l.domain(), l.PlistPath())
	}
	if err := l.launchctl(ctx, "bootstrap", l.domain(), l.PlistPath()); err != nil {
		if l.Sandboxed {
			return noteNextLogin + noteNextLoginE, nil
		}
		return "", err
	}
	return noteInstalled, nil
}

// writePlist puts the plist and the log directory in place and reports whether the plist changed.
func (l *Launchd) writePlist(plist []byte) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(l.LogPath()), 0o755); err != nil { //nolint:gosec // G301: the user's own logs
		return false, fmt.Errorf("service: %w", err)
	}
	old, _ := os.ReadFile(l.PlistPath())
	if err := writeAtomic(l.PlistPath(), plist, 0o644); err != nil {
		return false, err
	}
	return !bytes.Equal(old, plist), nil
}

// loaded reports whether launchd has the agent loaded.
func (l *Launchd) loaded(ctx context.Context) bool {
	_, loaded := l.loadedProgram(ctx)
	return loaded
}

// loadedProgram reports whether launchd has the agent loaded and, when launchctl says, the program that its loaded
// definition runs.
func (l *Launchd) loadedProgram(ctx context.Context) (string, bool) {
	out, err := l.Run(ctx, "launchctl", "print", l.target())
	if err != nil {
		return "", false
	}
	for line := range strings.SplitSeq(out, "\n") {
		if program, ok := strings.CutPrefix(line, "\tprogram = "); ok {
			return strings.TrimSpace(program), true
		}
	}
	return "", true
}

// bootout unloads the agent and waits until launchd has let it go, for goneWait at most. launchctl answers 36,
// "Operation now in progress", while the job is still exiting: that is a bootout under way, not a failure.
func (l *Launchd) bootout(ctx context.Context) error {
	out, err := l.Run(ctx, "launchctl", "bootout", l.target())
	if err != nil && !strings.Contains(out, "Operation now in progress") {
		return fmt.Errorf("service: launchctl bootout: %s (%w)", strings.TrimSpace(out), err)
	}
	deadline := time.Now().Add(goneWait)
	for l.loaded(ctx) {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("service: the taraengine agent is still exiting after %s; `taraengine service status` shows it",
				goneWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(gonePoll):
		}
	}
	return nil
}

// disabled reports whether a stop disabled the agent.
func (l *Launchd) disabled(ctx context.Context) (bool, error) {
	out, err := l.Run(ctx, "launchctl", "print-disabled", l.domain())
	if err != nil {
		return false, fmt.Errorf("service: launchctl print-disabled: %s (%w)", strings.TrimSpace(out), err)
	}
	return strings.Contains(out, `"`+Label+`" => disabled`) || strings.Contains(out, `"`+Label+`" => true`), nil
}

func (l *Launchd) installed() bool { return exists(l.PlistPath()) }

// Start enables the agent and runs it.
func (l *Launchd) Start(ctx context.Context) error {
	if !l.installed() {
		return ErrNotInstalled
	}
	if err := l.launchctl(ctx, "enable", l.target()); err != nil {
		return err
	}
	if l.loaded(ctx) {
		return l.launchctl(ctx, "kickstart", l.target())
	}
	return l.launchctl(ctx, "bootstrap", l.domain(), l.PlistPath())
}

// Stop disables the agent, so that it stays stopped across logins until a start, and then unloads it. The stop is
// written first, so that it holds even when the job takes its time to exit or the unload fails. A kill would not do:
// KeepAlive starts a killed agent again while its binary exists.
func (l *Launchd) Stop(ctx context.Context) error {
	if err := l.launchctl(ctx, "disable", l.target()); err != nil {
		return err
	}
	if l.loaded(ctx) {
		return l.bootout(ctx)
	}
	return nil
}

// Restart restarts a loaded agent and starts one that is not.
func (l *Launchd) Restart(ctx context.Context) error {
	if l.loaded(ctx) {
		return l.launchctl(ctx, "kickstart", "-k", l.target())
	}
	return l.Start(ctx)
}

// Uninstall unloads the agent, forgets a stop, and removes the plist. The data, the config and the logs stay.
func (l *Launchd) Uninstall(ctx context.Context) (string, error) {
	if !l.installed() {
		return "", ErrNotInstalled
	}
	if l.loaded(ctx) {
		if err := l.bootout(ctx); err != nil {
			return "", err
		}
	}
	if err := l.launchctl(ctx, "enable", l.target()); err != nil {
		return "", err
	}
	if err := os.Remove(l.PlistPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("service: %w", err)
	}
	return "removed the taraengine service; the models and the configuration stay, and its log is in " +
		filepath.Dir(l.LogPath()), nil
}

// State reads the agent's state from the plist and launchctl.
func (l *Launchd) State(ctx context.Context) (State, error) {
	st := State{Installed: l.installed(), Definition: l.PlistPath(), Logs: []string{l.LogPath()}}
	disabled, err := l.disabled(ctx)
	if err != nil {
		return st, err
	}
	st.Enabled = st.Installed && !disabled
	out, err := l.Run(ctx, "launchctl", "print", l.target())
	if err != nil {
		return st, nil // not loaded
	}
	st.Loaded = true
	if m := printState.FindStringSubmatch(out); m != nil {
		st.Running = m[1] == "running"
	}
	if m := printPID.FindStringSubmatch(out); m != nil && st.Running {
		st.PID, _ = strconv.Atoi(m[1])
	}
	if m := printLastExit.FindStringSubmatch(out); m != nil {
		st.LastExit = m[1]
	}
	return st, nil
}
