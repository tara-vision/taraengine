//go:build unix

package llamaserver

import (
	"fmt"
	"os/exec"
	"syscall"
)

// ownProcessGroup puts the child in its own group so a stop reaches the model processes the router spawns.
func ownProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// signalGroup sends sig to the process group led by pid. A pid of 1 or below is refused: 0 would reach
// the daemon's own group, 1 would reach every process the user may signal, and a negative pid would
// reach one unrelated process.
func signalGroup(pid int, sig syscall.Signal) error {
	if pid <= 1 {
		return fmt.Errorf("llamaserver: refusing to signal process group %d", pid)
	}
	return syscall.Kill(-pid, sig)
}
