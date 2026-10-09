package llamaserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/llamatest"
)

func TestUnloadWaitsUntilTheRouterHasStoppedTheModel(t *testing.T) {
	h := newHarnessScripted(t, llamatest.Options{UnloadDelay: 150 * time.Millisecond}, nil)
	ctx := context.Background()
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.b.Unload(ctx, "a"); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if st := h.fake.Status("a"); st != "unloaded" {
		t.Fatalf("the unload returned while the router still ran the model: %s", st)
	}
	if a := statusOf(t, h.b, "a"); a.Status != backend.StatusPulled || a.Memory != nil || a.Props != nil {
		t.Fatalf("a after the unload: %+v", a)
	}
}

func TestUnloadGivesUpOnARouterThatKeepsTheModel(t *testing.T) {
	h := newHarnessScripted(t, llamatest.Options{UnloadDelay: time.Minute}, nil)
	h.b.unloadWait = 100 * time.Millisecond
	ctx := context.Background()
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	err := h.b.Unload(ctx, "a")
	if err == nil || !strings.Contains(err.Error(), "still runs a") {
		t.Fatalf("an unload the router does not finish must be reported: %v", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("the wait took %v; it is bounded by the unload wait", took)
	}
}

func TestUnloadReportsAStatusItCannotRead(t *testing.T) {
	h := behindFlaky(t, failPlainStatusFrom(2)) // the start's reload is the first read of /models
	if err := h.b.Unload(context.Background(), "a"); err == nil || !strings.Contains(err.Error(), "scripted failure") {
		t.Fatalf("the wait after the unload must report a router it cannot read: %v", err)
	}
}

func TestLoadOfAModelTheRouterRunsRecordsItsFigures(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.SetStatus("a", "loaded") // an autoload: a chat request brought the model up
	ctx := context.Background()
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if a := statusOf(t, h.b, "a"); a.Memory == nil || a.Props == nil || a.Props.ContextLength != 4096 {
		t.Fatalf("a load of a model that already runs records what nothing recorded yet: %+v", a)
	}
	if n := countRequests(h.fake, "POST /models/load"); n != 0 {
		t.Fatalf("a running model is not loaded again, loads %d", n)
	}
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.probe.pids); n != 1 {
		t.Fatalf("figures already recorded are kept, the probe ran %d times", n)
	}
}

func TestLoadEndsWhenTheRouterDropsTheModelItWasLoading(t *testing.T) {
	h := newHarness(t, 0)
	h.b.opts.LoadTimeout = time.Minute
	h.fake.HangLoad("a")
	done := make(chan error, 1)
	go func() { done <- h.b.Load(context.Background(), "a", backend.LoadOptions{}) }()
	// The start's reload and the load's first read come before the load call. A fourth read means the
	// third, the first after the load call, has been answered, and it found the model loading.
	eventually(t, 5*time.Second, "the wait to see the model loading", func() bool {
		return countRequests(h.fake, "POST /models/load") == 1 && countRequests(h.fake, "GET /models") >= 4
	})
	h.fake.SetStatus("a", "unloaded") // another client's load evicted it
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped loading a") || errors.Is(err, backend.ErrLoadFailed) {
			t.Fatalf("want an error that says the router dropped the load, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the wait went on after the router dropped the model")
	}
	if st, _ := h.store.Load(); st.FailedLoads["a"] != 0 {
		t.Fatalf("a model another load pushed out did not fail: %+v", st)
	}
}

func TestCancelledContextChangeStopsTheLoadItGaveUp(t *testing.T) {
	h := newHarness(t, 0)
	h.b.opts.Poll = time.Minute // the context, not the poll, must end the wait
	h.fake.HangLoad("a")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := h.b.Load(ctx, "a", backend.LoadOptions{Context: 8192}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context's error, got %v", err)
	}
	if n := countRequests(h.fake, "POST /models/unload"); n != 1 {
		t.Fatalf("the load that was given up must be stopped, unloads %d", n)
	}
}

func TestCancelledContextChangeLogsAnUnloadThatFailed(t *testing.T) {
	var logs syncBuffer
	h := newHarnessWith(t, 0, func(fake *llamatest.Server, o *Options) {
		loggingTo(&logs)(fake, o)
		o.Attach = newFlakyRouter(t, fake.URL(), failFrom("/models/unload", 1)).URL
	})
	h.b.opts.Poll = time.Minute
	h.fake.HangLoad("a")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := h.b.Load(ctx, "a", backend.LoadOptions{Context: 8192})
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "scripted failure") {
		t.Fatalf("the courtesy unload is not the load's error: %v", err)
	}
	if !strings.Contains(logs.String(), "could not stop the load that was given up") {
		t.Fatalf("the unload that failed must be logged:\n%s", logs.String())
	}
	requireWindow(t, h, "a", 4096)
}
