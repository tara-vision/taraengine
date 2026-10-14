// Package service runs taraengine as a service: a LaunchAgent of the user on macOS, a systemd system service on Linux.
// It renders the definitions from templates and drives launchctl and systemctl through a Runner, so that every command
// can be tested against a fake on any system.
package service

import (
	"context"
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

// State is what the service manager says about the service.
type State struct {
	Installed  bool     // the definition is in place
	Enabled    bool     // it starts at login or boot; false once a stop disabled it
	Running    bool     // the manager runs it now
	PID        int      // its process, when it runs
	LastExit   string   // how it last ended, as the manager says it, when it knows
	Definition string   // the path of the plist or the unit
	Logs       []string // where to read what it wrote
}
