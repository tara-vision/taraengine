//go:build unix

package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend/llamaserver"
	"github.com/tara-vision/taraengine/internal/llamatest"
)

func TestWireLeavesTheIndexAloneWhileAnotherDaemonHoldsTheDataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	index := importedByAnOlderVersion(t, dataDir, "short", 2048)
	before, err := os.ReadFile(index.Path)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(llamaserver.LockPath(dataDir), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	fake := llamatest.New(t, llamatest.Options{APIKey: "k", PresetPath: llamaserver.PresetPath(dataDir)})
	_, err = Wire(context.Background(), attachedConfig(dataDir),
		WireOptions{Attach: fake.URL(), AttachKey: "k", Logger: debugLogger(io.Discard)})
	if err == nil || !strings.Contains(err.Error(), "in use by another taraengine") {
		t.Fatalf("a held data directory stops the start: %v", err)
	}
	if after, err := os.ReadFile(index.Path); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the index is left byte for byte as it was: %v\n%s", err, after)
	}
}
