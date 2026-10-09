package llamaserver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// groupPoll is the pause between two looks at a process group that is being ended.
const groupPoll = 50 * time.Millisecond

// stopLeftover ends a llama-server that an earlier daemon left running, as one that was killed or crashed
// does: the pid file names it, it is alive, and it is this supervisor's kind of child. Its process group,
// with the model processes and the memory they hold, gets SIGTERM and, once the grace has passed, SIGKILL.
// A pid file that names anything else is left alone, because that pid may belong to another process by
// now. The wait ends early, with the reason, when the caller's context ends or Stop is called.
func (s *Supervisor) stopLeftover(ctx context.Context) error {
	pid, ok := s.leftover(ctx)
	if !ok {
		return nil
	}
	s.opts.Logger.Warn("stopping a llama-server left over from an earlier run", "pid", pid)
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		gone, err := s.endGroup(ctx, pid, sig)
		if err != nil || gone {
			return err
		}
	}
	s.opts.Logger.Warn("the llama-server left over from an earlier run is still there", "pid", pid)
	return nil
}

// leftover is the pid the pid file names, when that process runs this runtime's binary with this child's
// preset argument, which names the data directory. A router of another data directory that shares the
// runtime, named by a stale or copied pid file, is not ours; a child started without a preset argument
// never has a leftover.
func (s *Supervisor) leftover(ctx context.Context) (int, bool) {
	preset := presetArgument(s.opts.Args)
	if s.opts.PIDFile == "" || preset == "" {
		return 0, false
	}
	data, err := os.ReadFile(s.opts.PIDFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return 0, false
	}
	command, err := processCommand(ctx, pid)
	return pid, err == nil && strings.HasPrefix(command+" ", s.opts.Binary+" "+preset+" ")
}

// presetArgument is the "--models-preset <path>" pair of the child's arguments, or "" when there is none.
func presetArgument(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--models-preset" {
			return args[i] + " " + args[i+1]
		}
	}
	return ""
}

// processCommand is the command line of a process as ps shows it, run without a shell; there is an error
// when no such process exists.
func processCommand(ctx context.Context, pid int) (string, error) {
	out, err := exec.CommandContext(ctx, "ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out)), err
}

// endGroup sends sig to the process group and reports whether the group is gone within the grace. A group
// that cannot be signalled counts as still there, unless it is gone already. The wait stops with an error
// when the caller's context ends or Stop is called.
func (s *Supervisor) endGroup(ctx context.Context, pid int, sig syscall.Signal) (bool, error) {
	if err := s.sendSignal(pid, sig); err != nil {
		return errors.Is(err, syscall.ESRCH), nil
	}
	deadline := time.Now().Add(s.opts.Grace)
	for !errors.Is(s.sendSignal(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			return false, nil
		}
		if err := s.pause(ctx, groupPoll); err != nil {
			return false, err
		}
	}
	return true, nil
}

// pause waits for d. It returns early with the context's error when the caller's context ends, and with
// the stopping error once Stop has been called.
func (s *Supervisor) pause(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return errStopping
	case <-time.After(d):
		return nil
	}
}

// killGroup sends SIGKILL to the process group of a child that has exited: the model processes the router
// started may still run and hold memory, and a group id is given to no one else while one of them lives.
func (s *Supervisor) killGroup(pid int) {
	if err := s.sendSignal(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		s.opts.Logger.Warn("could not end the process group of the exited llama-server", "pid", pid, "error", err)
	}
}

// writePIDFile records the pid of the child that just started. A file that cannot be written costs only
// that safety net, so it is logged and the child runs on.
func (s *Supervisor) writePIDFile(pid int) {
	if s.opts.PIDFile == "" {
		return
	}
	if err := replaceWithPrivateFile(s.opts.PIDFile, []byte(strconv.Itoa(pid)+"\n")); err != nil {
		s.opts.Logger.Warn("could not write the pid file", "path", s.opts.PIDFile, "error", err)
	}
}

// removePIDFile drops the pid file once no child of this supervisor runs.
func (s *Supervisor) removePIDFile() {
	if s.opts.PIDFile != "" {
		_ = os.Remove(s.opts.PIDFile)
	}
}
