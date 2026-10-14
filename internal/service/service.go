// Package service runs taraengine as a service: a LaunchAgent of the user on macOS, a systemd system service on Linux.
// It renders the definitions from templates and drives launchctl and systemctl through a Runner, so that every command
// can be tested against a fake on any system.
package service

import (
	"context"
	"errors"
	"os/exec"
)

// Label is the launchd label of the agent, and UnitName the systemd unit.
const (
	Label    = "vision.tara.taraengine"
	UnitName = "taraengine.service"
)

// Runner runs a command and returns what it printed, standard output and standard error together.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner runs the command for real.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// ErrNotInstalled is the answer of a command that needs the service installed.
var ErrNotInstalled = errors.New("the taraengine service is not installed; `taraengine service install` installs it")

// Manager installs and drives the service on one system.
type Manager interface {
	// Install writes the definition for binary and makes the service run it: it starts a service that is not running
	// and restarts one that is, unless the service was stopped on purpose. It returns a sentence on what it did.
	Install(ctx context.Context, binary string) (string, error)
	// Uninstall stops the service and removes the definition, and says where what it leaves behind is.
	Uninstall(ctx context.Context) (string, error)
	Start(ctx context.Context) error   // enables and starts it
	Stop(ctx context.Context) error    // stops and disables it, until a start
	Restart(ctx context.Context) error // restarts it, or starts it when it does not run
	State(ctx context.Context) (State, error)
}

// State is what the service manager says about the service.
type State struct {
	Installed  bool     // the definition is in place
	Loaded     bool     // the manager has the definition loaded
	Enabled    bool     // it starts at login or boot; false once a stop disabled it
	Running    bool     // the manager runs it now
	PID        int      // its process, when it runs
	LastExit   string   // how it last ended, as the manager says it, when it knows
	Definition string   // the path of the plist or the unit
	Logs       []string // where to read what it wrote
}
