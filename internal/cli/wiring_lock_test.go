package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend/llamaserver"
	"github.com/tara-vision/taraengine/internal/llamatest"
)

func TestWireHandsTheBackendTheLockItIsGiven(t *testing.T) {
	dataDir := t.TempDir()
	lock, err := llamaserver.LockDataDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	fake := llamatest.New(t, llamatest.Options{APIKey: "k", PresetPath: llamaserver.PresetPath(dataDir)})
	wired, err := Wire(context.Background(), attachedConfig(dataDir), WireOptions{
		Attach: fake.URL(), AttachKey: "k", Logger: debugLogger(io.Discard), HeldLock: lock,
	})
	if err != nil {
		t.Fatalf("a daemon that took the lock first wires on it: %v", err)
	}
	if err := wired.Backend.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("the backend let go of the lock when it stopped: %v", err)
	}
}
