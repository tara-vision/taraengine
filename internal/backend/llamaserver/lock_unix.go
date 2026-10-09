//go:build unix

package llamaserver

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// flock is syscall.Flock; a variable only so the tests can reach a file system that refuses the lock.
var flock = syscall.Flock

// lockDataDir takes the exclusive advisory lock on the data directory's lock file, held for as long as the
// returned file stays open, and writes this process's pid into it. When another process holds the lock,
// the error names the data directory and that process's pid. The file is never removed: a lock file
// that went away while another daemon waited on it would let two daemons hold a lock each.
func lockDataDir(dataDir string) (*os.File, error) {
	f, err := os.OpenFile(LockPath(dataDir), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("llamaserver: open the data directory lock: %w", err)
	}
	if err := flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := lockHolder(f)
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("llamaserver: the data directory %s is in use by another taraengine (pid %s)",
				dataDir, holder)
		}
		return nil, fmt.Errorf("llamaserver: lock the data directory %s: %w", dataDir, err)
	}
	// The pid only names this daemon to a second one; the lock holds without it.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return f, nil
}

// lockHolder is the pid the holder of the lock wrote into the file, or "unknown".
func lockHolder(f *os.File) string {
	data, _ := io.ReadAll(io.LimitReader(f, 32))
	holder := strings.TrimSpace(string(data))
	if pid, err := strconv.Atoi(holder); err != nil || pid <= 0 {
		return "unknown"
	}
	return holder
}
