package llamaserver

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// secondDaemon is a backend on the data directory of s, as a second serve would build it. It counts the
// children it would start.
func secondDaemon(t *testing.T, s *supervised) (*Backend, *fakeProcess, *int) {
	t.Helper()
	b, err := New(Options{
		RuntimeDir: s.runtimeDir, DataDir: s.dataDir, Port: s.b.opts.Port, APIKey: "secret", // the key the scripted router takes
		SleepIdleSeconds: 900, Poll: 10 * time.Millisecond, State: testStore(s.dataDir), Logger: quietLogger(),
		Models: s.b.opts.Models,
	})
	if err != nil {
		t.Fatal(err)
	}
	proc, built := &fakeProcess{pid: 8888}, 0
	b.newProcess = func(o SupervisorOptions) childProcess {
		built++
		proc.mu.Lock()
		proc.opts = o
		proc.mu.Unlock()
		return proc
	}
	t.Cleanup(func() { _ = b.Stop(context.Background()) })
	return b, proc, &built
}

func readFileOrEmpty(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func TestASecondDaemonOnTheDataDirectoryIsRefusedAndTouchesNothing(t *testing.T) {
	s := newSupervised(t)
	ctx := context.Background()
	if err := s.b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, PIDPath(s.dataDir), "4242\n")
	files := []string{PIDPath(s.dataDir), PresetPath(s.dataDir), KeyPath(s.dataDir)}
	before := make([]string, 0, len(files))
	for _, f := range files {
		before = append(before, readFileOrEmpty(f))
	}
	second, _, built := secondDaemon(t, s)
	began := time.Now()
	err := second.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), s.dataDir) || !strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("the refusal names the data directory and the daemon that holds it: %v", err)
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("the refusal took %v; it must not wait for anything", took)
	}
	if *built != 0 {
		t.Fatalf("the second daemon built %d children", *built)
	}
	if starts, stops := s.proc.counts(); starts != 1 || stops != 0 || !s.proc.Running() {
		t.Fatalf("the first daemon's router must run on untouched: starts %d, stops %d", starts, stops)
	}
	for i, f := range files {
		if got := readFileOrEmpty(f); got != before[i] {
			t.Errorf("%s changed from %q to %q", f, before[i], got)
		}
	}
	if a := statusOf(t, s.b, "a"); a.Status != "pulled" {
		t.Fatalf("the first daemon still serves: %+v", a)
	}
}

func TestStopFreesTheDataDirectoryLock(t *testing.T) {
	s := newSupervised(t)
	ctx := context.Background()
	if err := s.b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.b.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	second, proc, _ := secondDaemon(t, s)
	if err := second.Start(ctx); err != nil {
		t.Fatalf("once the first daemon stopped, the data directory is free: %v", err)
	}
	if starts, _ := proc.counts(); starts != 1 {
		t.Fatalf("the second daemon starts its own child, starts %d", starts)
	}
}

func TestAFailedStartFreesTheDataDirectoryLock(t *testing.T) {
	s := newSupervised(t)
	s.proc.startErr = errors.New("exited before it became healthy")
	ctx := context.Background()
	if err := s.b.Start(ctx); err == nil {
		t.Fatal("the failing child must fail the start")
	}
	second, _, _ := secondDaemon(t, s)
	if err := second.Start(ctx); err != nil {
		t.Fatalf("a start that failed holds no lock: %v", err)
	}
}

func TestTheLockFileNamesTheDaemonAndIsPrivate(t *testing.T) {
	h := newHarness(t, 0)
	if got := readFileOrEmpty(LockPath(h.b.opts.DataDir)); got != strconv.Itoa(os.Getpid())+"\n" {
		t.Fatalf("lock file %q", got)
	}
	if info, err := os.Stat(LockPath(h.b.opts.DataDir)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file %v: %v", info, err)
	}
	if err := h.b.Start(context.Background()); err != nil {
		t.Fatalf("a backend that holds the lock may sync again: %v", err)
	}
	other, err := New(Options{DataDir: h.b.opts.DataDir, Attach: h.fake.URL(), APIKey: "k", SleepIdleSeconds: 900,
		State: h.store, Logger: quietLogger(), Models: h.b.opts.Models})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "in use by another taraengine") {
		t.Fatalf("an attached backend holds the data directory as well: %v", err)
	}
}

// holdLock takes the lock of the data directory from outside the backend and writes content into the file.
func holdLock(t *testing.T, dataDir, content string) {
	t.Helper()
	f, err := os.OpenFile(LockPath(dataDir), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func TestStartReportsALockItCannotTake(t *testing.T) {
	t.Run("a holder that wrote no pid", func(t *testing.T) {
		s := newSupervised(t)
		holdLock(t, s.dataDir, "not a pid")
		err := s.b.Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), "pid unknown") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a lock path that is a directory", func(t *testing.T) {
		s := newSupervised(t)
		if err := os.Mkdir(LockPath(s.dataDir), 0o750); err != nil {
			t.Fatal(err)
		}
		err := s.b.Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), "data directory lock") {
			t.Fatalf("got %v", err)
		}
		if _, err := os.Stat(PresetPath(s.dataDir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a start without the lock writes nothing: %v", err)
		}
	})
	t.Run("a file system that refuses the lock", func(t *testing.T) {
		original := flock
		flock = func(int, int) error { return syscall.ENOLCK }
		t.Cleanup(func() { flock = original })
		s := newSupervised(t)
		err := s.b.Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), "lock the data directory") || strings.Contains(err.Error(), "in use") {
			t.Fatalf("got %v", err)
		}
	})
}
