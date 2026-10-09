//go:build linux || darwin

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/tara-vision/taraengine/pkg/api"
)

func TestPullRefusesANamedPipeWithoutOpeningIt(t *testing.T) {
	h := newHarness(t, "")
	pipe := filepath.Join(t.TempDir(), "Pipe.gguf")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	// Opening a pipe for reading waits for a writer. Should the engine do that, a writer frees it, so that the
	// test can end instead of leaving the handler stuck.
	t.Cleanup(func() {
		if w, err := os.OpenFile(pipe, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	})
	resp, data := h.do(t, http.MethodPost, "/api/pull", "", api.PullRequest{Model: pipe})
	if body := decode[api.Error](t, data).Error; resp.StatusCode != http.StatusNotFound || !strings.Contains(body.Message, "Pipe.gguf is not a readable file") {
		t.Fatalf("a named pipe: %d %s", resp.StatusCode, data)
	}
}
