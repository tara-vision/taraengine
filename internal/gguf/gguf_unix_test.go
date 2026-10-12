//go:build linux || darwin

package gguf

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadInfoNeverOpensANamedPipe(t *testing.T) {
	pipe := filepath.Join(t.TempDir(), "pipe.gguf")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	// Opening a pipe for reading waits for a writer. Should the reader do that, a writer frees it, so that the test
	// can end instead of leaving the reader stuck.
	t.Cleanup(func() {
		if w, err := os.OpenFile(pipe, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := ReadInfo(pipe)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("a named pipe: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the reader opened a named pipe and waits for a writer")
	}
}
