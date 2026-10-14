package llamaserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// handedLock is the data directory's lock of s, taken the way serve takes it before the backend starts.
func handedLock(t *testing.T, s *supervised) *os.File {
	t.Helper()
	lock, err := LockDataDir(s.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	return lock
}

// closed reports whether the file was closed: a closed *os.File answers every call with os.ErrClosed.
func closed(f *os.File) bool {
	_, err := f.Stat()
	return errors.Is(err, os.ErrClosed)
}

func TestLockDataDirCreatesTheDirectoryAndRefusesASecondHolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not", "there", "yet")
	lock, err := LockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("the data directory is created privately: %v %v", info, err)
	}
	if _, err := LockDataDir(dir); err == nil || !strings.Contains(err.Error(), "in use by another taraengine") {
		t.Fatalf("a second holder is refused: %v", err)
	}
}

func TestLockDataDirReportsADirectoryItCannotCreate(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a-file")
	writeTestFile(t, file, "x")
	if _, err := LockDataDir(filepath.Join(file, "data")); err == nil || !strings.HasPrefix(err.Error(), "llamaserver: ") {
		t.Fatalf("got %v", err)
	}
}

func TestStartAdoptsTheLockItIsHandedAndStopLetsItGo(t *testing.T) {
	s := newSupervised(t)
	lock := handedLock(t, s)
	b, _, _ := secondDaemon(t, s)
	b.heldLock = lock
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("a backend that is handed the lock starts on it: %v", err)
	}
	if err := b.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if !closed(lock) {
		t.Fatal("Stop lets go of the lock it was handed")
	}
	if err := b.Start(ctx); err != nil {
		t.Fatalf("a later Start takes a lock of its own: %v", err)
	}
}

func TestAFailedStartLetsGoOfTheLockItWasHanded(t *testing.T) {
	s := newSupervised(t)
	s.proc.startErr = errors.New("exited before it became healthy")
	lock := handedLock(t, s)
	s.b.heldLock = lock
	if err := s.b.Start(context.Background()); err == nil {
		t.Fatal("the failing child must fail the start")
	}
	if !closed(lock) {
		t.Fatal("a start that failed holds no lock, not even one it was handed")
	}
	second, _, _ := secondDaemon(t, s)
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("the data directory is free again: %v", err)
	}
}

func TestNewTakesTheHeldLockFromTheOptions(t *testing.T) {
	s := newSupervised(t)
	lock := handedLock(t, s)
	b, err := New(Options{
		RuntimeDir: s.runtimeDir, DataDir: s.dataDir, APIKey: "secret", SleepIdleSeconds: 900,
		Poll: 10 * time.Millisecond, State: testStore(s.dataDir), Logger: quietLogger(), Models: s.b.opts.Models,
		HeldLock: lock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.heldLock != lock {
		t.Fatal("New keeps the lock the options hand over")
	}
}
