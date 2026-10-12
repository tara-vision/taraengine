package llamaserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend"
)

// loadInBackground starts a load of a on another goroutine and returns once the load has read the router twice
// more, so that it found the model loading and is waiting for it, or has ended.
func loadInBackground(t *testing.T, h *harness, opts backend.LoadOptions) <-chan error {
	t.Helper()
	before := countRequests(h.fake, "GET /models")
	done := make(chan error, 1)
	go func() { done <- h.b.Load(context.Background(), "a", opts) }()
	eventually(t, 5*time.Second, "the load waiting for the model", func() bool {
		return len(done) > 0 || countRequests(h.fake, "GET /models") >= before+2
	})
	return done
}

// loadResult waits for a load started in the background, for at most five seconds.
func loadResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the load did not end")
		return nil
	}
}

func TestLoadWaitsForAModelThatIsLoadingAndRecordsIt(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.SetStatus("a", "loading") // a request started it
	done := loadInBackground(t, h, backend.LoadOptions{})
	h.fake.SetStatus("a", "loaded")
	if err := loadResult(t, done); err != nil {
		t.Fatalf("a load of a model that is loading waits for it: %v", err)
	}
	if n := countRequests(h.fake, "POST /models/load"); n != 0 {
		t.Fatalf("the router refuses a load of a model it runs, so the engine sends none: %d loads", n)
	}
	if len(h.probe.pids) != 1 || countRequests(h.fake, "GET /props") != 1 {
		t.Fatalf("the load records the model as if it had started it: probes %v", h.probe.pids)
	}
	if st, _ := h.store.Load(); st.LastLoaded != "a" {
		t.Fatalf("state after the awaited load: %+v", st)
	}
	if a := statusOf(t, h.b, "a"); a.Status != backend.StatusLoaded || a.Memory == nil || a.Props == nil {
		t.Fatalf("a: %+v", a)
	}
}

func TestLoadWaitsForALoadingModelThatFallsAsleep(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.SetStatus("a", "loading")
	done := loadInBackground(t, h, backend.LoadOptions{})
	h.fake.SetStatus("a", "sleeping") // up and asleep between two polls, with a short sleep_idle_seconds
	if err := loadResult(t, done); err != nil {
		t.Fatal(err)
	}
	if len(h.probe.pids) != 0 {
		t.Fatalf("a model that is asleep is not measured: %v", h.probe.pids)
	}
	if a := statusOf(t, h.b, "a"); a.Status != backend.StatusSleeping || a.Props == nil || a.Memory != nil {
		t.Fatalf("a model asleep when the wait ends gets its props only: %+v", a)
	}
}

func TestALoadThatFailsWhileAwaitedCountsAgainstTheModel(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.SetStatus("a", "loading")
	done := loadInBackground(t, h, backend.LoadOptions{})
	h.fake.SetFailed("a", 137)
	err := loadResult(t, done)
	if !errors.Is(err, backend.ErrLoadFailed) || !strings.Contains(err.Error(), "exit code 137") {
		t.Fatalf("want the failed load, got %v", err)
	}
	if st, _ := h.store.Load(); st.FailedLoads["a"] != 1 {
		t.Fatalf("the failure counts against the model: %+v", st)
	}
}

func TestALoadThatTheRouterDropsWhileAwaitedIsReported(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.SetStatus("a", "loading")
	done := loadInBackground(t, h, backend.LoadOptions{})
	h.fake.SetStatus("a", "unloaded") // another load evicted it before it was up
	err := loadResult(t, done)
	if err == nil || !strings.Contains(err.Error(), "stopped loading a") || errors.Is(err, backend.ErrLoadFailed) {
		t.Fatalf("want an error that says the router dropped the load, got %v", err)
	}
	if st, _ := h.store.Load(); st.FailedLoads["a"] != 0 {
		t.Fatalf("a model another load pushed out did not fail: %+v", st)
	}
}

func TestASleepingModelAtAnotherWindowIsLoadedAgain(t *testing.T) {
	h := newHarness(t, 0)
	ctx := context.Background()
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	h.fake.SetStatus("a", "sleeping")
	if err := h.b.Load(ctx, "a", backend.LoadOptions{Context: 8192}); err != nil {
		t.Fatal(err)
	}
	a := statusOf(t, h.b, "a")
	if a.Status != backend.StatusLoaded || a.Context != 8192 || a.Props == nil || a.Props.ContextLength != 8192 {
		t.Fatalf("a sleeping model asked for another window runs at it: %+v", a)
	}
	if n := countRequests(h.fake, "POST /models/load"); n != 2 {
		t.Fatalf("loads %d, want 2", n)
	}
}

func TestALoadingModelAtAnotherWindowIsLoadedAgain(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.SetStatus("a", "loading") // a request started it at the window of its preset
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{Context: 8192}); err != nil {
		t.Fatalf("the reload of the presets stops the load under way, and the model loads at the new window: %v", err)
	}
	a := statusOf(t, h.b, "a")
	if a.Status != backend.StatusLoaded || a.Context != 8192 || a.Props == nil || a.Props.ContextLength != 8192 {
		t.Fatalf("a: %+v", a)
	}
	if n := countRequests(h.fake, "POST /models/load"); n != 1 {
		t.Fatalf("loads %d, want 1", n)
	}
}
