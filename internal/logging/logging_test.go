package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr points os.Stderr at a file for the rest of the test and returns the file's path, so that what
// Setup sends to the terminal can be read back and the test output stays clean.
func captureStderr(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stderr.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = previous
		_ = f.Close()
	})
	return path
}

func TestSetupWritesTheRotatingFile(t *testing.T) {
	captureStderr(t)
	dir := t.TempDir()
	logger, closeFn, err := Setup(dir)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello", "k", "v")
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "logs", "taraengine.log"))
	if err != nil || !strings.Contains(string(data), "hello") || !strings.Contains(string(data), "k=v") {
		t.Fatalf("log file: %v %q", err, data)
	}
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Setup(blocker); err == nil {
		t.Fatal("a data dir that is a file must fail")
	}
}

func TestSetupEchoesToStderrAtInfoLevel(t *testing.T) {
	stderr := captureStderr(t)
	logger, closeFn, err := Setup(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("too quiet to show")
	logger.Warn("shown", "listen", "127.0.0.1:8272")
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stderr)
	if err != nil || !strings.Contains(string(data), "level=WARN") || !strings.Contains(string(data), "listen=127.0.0.1:8272") {
		t.Fatalf("stderr: %v %q", err, data)
	}
	if strings.Contains(string(data), "too quiet") {
		t.Fatalf("debug lines must not be written: %q", data)
	}
}

func TestSetupKeepsTheLogPrivate(t *testing.T) {
	captureStderr(t)
	dir := t.TempDir()
	logger, closeFn, err := Setup(dir)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello")
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "logs"))
	if err != nil || !info.IsDir() || info.Mode().Perm()&^0o750 != 0 {
		t.Fatalf("the log directory must allow nothing beyond 0750: %v %v", info, err)
	}
	info, err = os.Stat(filepath.Join(dir, "logs", "taraengine.log"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the log file must be private: %v %v", info, err)
	}
}

func TestSetupRotatesAtTwentyMegabytes(t *testing.T) {
	captureStderr(t)
	dir := t.TempDir()
	logger, closeFn, err := Setup(dir)
	if err != nil {
		t.Fatal(err)
	}
	block := strings.Repeat("x", 1<<20)
	for range 21 {
		logger.Info("filler", "block", block)
	}
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("21 MB of log lines must leave the live file and one rotated file, got %d entries", len(entries))
	}
}
