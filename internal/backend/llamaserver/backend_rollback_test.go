package llamaserver

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/llamatest"
)

func overridesOf(t *testing.T, h *harness) map[string]int {
	t.Helper()
	st, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return st.Overrides
}

// failReloadsFrom lets the first n-1 reloads through, the first of which is the start's own, and refuses the rest.
func failReloadsFrom(n int) func(*http.Request, int) *cannedReply {
	var reloads atomic.Int32
	return func(r *http.Request, _ int) *cannedReply {
		if r.URL.Path == "/models" && r.URL.Query().Get("reload") == "1" && int(reloads.Add(1)) >= n {
			return scriptedFailure(http.StatusInternalServerError)
		}
		return nil
	}
}

// requireWindow checks that the preset file and the roster carry the window, and that the router's own
// copy of the preset does, which is what the next load asks for.
func requireWindow(t *testing.T, h *harness, name string, window int) {
	t.Helper()
	if !strings.Contains(readPresets(t, h), "["+name+"]\nc = "+strconv.Itoa(window)+"\n") {
		t.Errorf("the preset file must carry c = %d:\n%s", window, readPresets(t, h))
	}
	if m := statusOf(t, h.b, name); m.Context != window {
		t.Errorf("roster context %d, want %d", m.Context, window)
	}
	entry, err := h.b.routerStatus(context.Background(), name)
	if err != nil || !presetHasContext(entry.Preset, window) {
		t.Errorf("the router must hold the window %d: %v %q", window, err, entry.Preset)
	}
}

func TestFailedContextChangeBringsBackThePreviousWindow(t *testing.T) {
	h := newHarness(t, 0)
	ctx := context.Background()
	if err := h.b.Load(ctx, "a", backend.LoadOptions{Context: 2048}); err != nil {
		t.Fatal(err)
	}
	h.fake.FailLoad("a", 1)
	err := h.b.Load(ctx, "a", backend.LoadOptions{Context: 8192})
	if !errors.Is(err, backend.ErrLoadFailed) {
		t.Fatalf("want a failed load, got %v", err)
	}
	requireWindow(t, h, "a", 2048)
	if got := overridesOf(t, h)["a"]; got != 2048 {
		t.Fatalf("state holds the override %d, want the earlier 2048", got)
	}
	if st, _ := h.store.Load(); st.FailedLoads["a"] != 1 {
		t.Fatalf("the failed load still counts against the model: %+v", st)
	}
}

func TestFailedFirstContextChangeLeavesNoOverride(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.FailLoad("b", 1)
	if err := h.b.Load(context.Background(), "b", backend.LoadOptions{Context: 8192}); !errors.Is(err, backend.ErrLoadFailed) {
		t.Fatalf("want a failed load, got %v", err)
	}
	if kept, ok := overridesOf(t, h)["b"]; ok {
		t.Fatalf("a model that never had an override must not keep the failed one: %d", kept)
	}
	requireWindow(t, h, "b", 4096)
}

func TestRefusedLoadCallBringsBackThePreviousWindow(t *testing.T) {
	var loads atomic.Int32
	h := behindFlaky(t, func(r *http.Request, _ int) *cannedReply {
		if r.URL.Path == "/models/load" && loads.Add(1) == 1 {
			return scriptedFailure(http.StatusInternalServerError)
		}
		return nil
	})
	ctx := context.Background()
	if err := h.b.Load(ctx, "a", backend.LoadOptions{Context: 8192}); err == nil || !strings.Contains(err.Error(), "scripted failure") {
		t.Fatalf("the refused load: %v", err)
	}
	requireWindow(t, h, "a", 4096)
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatalf("the next plain load: %v", err)
	}
	if a := statusOf(t, h.b, "a"); a.Status != backend.StatusLoaded || a.Props == nil || a.Props.ContextLength != 4096 {
		t.Fatalf("the plain load must run at the old window: %+v", a)
	}
}

func TestTimedOutContextChangeBringsBackThePreviousWindow(t *testing.T) {
	h := newHarness(t, 0)
	h.b.opts.LoadTimeout = 150 * time.Millisecond
	h.fake.HangLoad("a")
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{Context: 8192}); !errors.Is(err, backend.ErrLoadTimeout) {
		t.Fatalf("want a timeout, got %v", err)
	}
	requireWindow(t, h, "a", 4096)
	if _, ok := overridesOf(t, h)["a"]; ok {
		t.Fatal("a timed out change must leave no override")
	}
}

func TestCancelledContextChangeBringsBackThePreviousWindow(t *testing.T) {
	h := newHarness(t, 0)
	h.b.opts.Poll = time.Minute // the context, not the poll, must end the wait
	h.fake.HangLoad("a")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := h.b.Load(ctx, "a", backend.LoadOptions{Context: 8192}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context's error, got %v", err)
	}
	requireWindow(t, h, "a", 4096) // the caller has given up, and the preset must still be put right
}

func TestContextTheRouterDoesNotPickUpIsNotKept(t *testing.T) {
	h := newHarnessWith(t, 0, func(_ *llamatest.Server, o *Options) {
		o.Models = func(context.Context, map[string]int) ([]PresetModel, error) {
			return []PresetModel{{Name: "a", ModelPath: "/m/a.gguf", Context: 4096}}, nil
		}
	})
	err := h.b.Load(context.Background(), "a", backend.LoadOptions{Context: 8192})
	if err == nil || !strings.Contains(err.Error(), "did not pick up the context 8192") {
		t.Fatalf("got %v", err)
	}
	if kept, ok := overridesOf(t, h)["a"]; ok {
		t.Fatalf("an override the preset cannot carry must not stay in state: %d", kept)
	}
}

func TestRollbackErrorsAreJoinedToTheLoadError(t *testing.T) {
	h := behindFlaky(t, failReloadsFrom(3))
	h.fake.FailLoad("b", 1)
	err := h.b.Load(context.Background(), "b", backend.LoadOptions{Context: 8192})
	if !errors.Is(err, backend.ErrLoadFailed) || !strings.Contains(err.Error(), "exit code 1") || !strings.Contains(err.Error(), "scripted failure") {
		t.Fatalf("the load error and the refused rollback reload must both show: %v", err)
	}
	if kept, ok := overridesOf(t, h)["b"]; ok {
		t.Fatalf("the state is rolled back even when the reload is refused: %d", kept)
	}
}

func TestLostStateAfterTheLoadRollsTheWindowBack(t *testing.T) {
	var h *harness
	h = behindFlaky(t, func(r *http.Request, _ int) *cannedReply {
		if r.URL.Path == "/models/load" { // the override is saved by now; every later save has to fail
			_ = os.Remove(h.store.Path)
			_ = os.Mkdir(h.store.Path, 0o750)
		}
		return nil
	})
	err := h.b.Load(context.Background(), "a", backend.LoadOptions{Context: 8192})
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("a state that cannot be saved must be reported: %v", err)
	}
	if !strings.Contains(readPresets(t, h), "[a]\nc = 4096\n") {
		t.Fatalf("the preset file goes back to the old window:\n%s", readPresets(t, h))
	}
	if m := statusOf(t, h.b, "a"); m.Context != 4096 {
		t.Fatalf("the roster goes back too: %+v", m)
	}
}

func TestOverrideThatCannotBeSavedIsNotKept(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	writeTestFile(t, blocker, "x")
	h := newHarness(t, 0)
	good := h.b.opts.State
	h.b.opts.State.Path = filepath.Join(blocker, "state.json")
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{Context: 8192}); err == nil {
		t.Fatal("a window that cannot be remembered must stop the load")
	}
	h.b.opts.State = good
	if err := h.b.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readPresets(t, h), "[a]\nc = 4096\n") {
		t.Fatalf("the unsaved window must not come back with the next refresh:\n%s", readPresets(t, h))
	}
}

func TestTimedOutLoadLogsAnUnloadThatFailed(t *testing.T) {
	var logs syncBuffer
	h := newHarnessWith(t, 0, func(fake *llamatest.Server, o *Options) {
		loggingTo(&logs)(fake, o)
		o.Attach = newFlakyRouter(t, fake.URL(), failFrom("/models/unload", 1)).URL
	})
	h.b.opts.LoadTimeout = 150 * time.Millisecond
	h.fake.HangLoad("a")
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{}); !errors.Is(err, backend.ErrLoadTimeout) {
		t.Fatalf("want a timeout, got %v", err)
	}
	if !strings.Contains(logs.String(), "could not cancel the load") || !strings.Contains(logs.String(), "scripted failure") {
		t.Fatalf("the unload that failed must be logged:\n%s", logs.String())
	}
}
