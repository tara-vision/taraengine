package llamaserver

import (
	"context"
	"errors"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/measure"
)

func TestModelsShowTheFiguresOfTheRunThatIsUp(t *testing.T) {
	h := newHarness(t, 0)
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	h.fake.SetStatus("a", "unloaded") // an eviction, or a reload that unloaded an edited section
	if a := statusOf(t, h.b, "a"); a.Memory != nil || a.Props != nil {
		t.Fatalf("a model that is not running shows no figures: %+v", a)
	}
	h.probe.mem = backend.Memory{Bytes: 3 << 30, Source: "measured"}
	h.fake.SetStatus("a", "loaded") // the router brings it back on its own, as an autoload does
	a := statusOf(t, h.b, "a")
	if a.Status != backend.StatusLoaded || a.Memory == nil || a.Memory.Bytes != 3<<30 || a.Props == nil {
		t.Fatalf("a model that came back shows the figures of its new run, never those of the earlier one: %+v", a)
	}
}

func TestModelsKeepTheFiguresOfAModelCaughtLoading(t *testing.T) {
	h := newHarness(t, 0)
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	h.fake.SetStatus("a", "loading") // a model woken from its sleep loads again
	if a := statusOf(t, h.b, "a"); a.Status != backend.StatusLoading || a.Memory == nil || a.Props == nil {
		t.Fatalf("a model caught loading keeps its figures: %+v", a)
	}
	h.fake.SetStatus("a", "loaded")
	if a := statusOf(t, h.b, "a"); a.Memory == nil || a.Props == nil {
		t.Fatalf("the figures are still there once it is up: %+v", a)
	}
}

func TestPropsAreReadWithoutLettingTheRouterLoad(t *testing.T) {
	h := newHarness(t, 0)
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if n, all := countRequests(h.fake, "GET /props?model=a&autoload=false"), countRequests(h.fake, "GET /props"); n != 1 || all != 1 {
		t.Fatalf("the props are read once, with autoload off: %d of %d\n%v", n, all, h.fake.Requests())
	}
}

func TestALoadOfARunningModelRecordsOnlyWhatIsMissing(t *testing.T) {
	h := newHarness(t, 0)
	ctx := context.Background()
	h.probe.err = errors.New("ps exploded")
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	h.probe.err = nil
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := countRequests(h.fake, "GET /props"); n != 1 {
		t.Fatalf("props already recorded are not read again: %d reads", n)
	}
	if len(h.probe.pids) != 2 {
		t.Fatalf("the memory that was missing is measured again: %v", h.probe.pids)
	}
	if a := statusOf(t, h.b, "a"); a.Memory == nil || a.Props == nil {
		t.Fatalf("a: %+v", a)
	}
}

func TestAMemoryFigureTheRunCannotGiveIsTriedOncePerRun(t *testing.T) {
	h := newHarness(t, 0)
	ctx := context.Background()
	h.probe.err = measure.ErrNoFigure // as when nvidia-smi cannot see the router's processes
	for range 3 {
		if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.probe.pids) != 1 {
		t.Fatalf("a run whose memory the probe cannot see is probed once: %v", h.probe.pids)
	}
	if err := h.b.Unload(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(h.probe.pids) != 2 {
		t.Fatalf("the next run is probed again: %v", h.probe.pids)
	}
}

func TestAMemoryProbeThatBreaksIsTriedAgain(t *testing.T) {
	h := newHarness(t, 0)
	ctx := context.Background()
	h.probe.err = errors.New("ps exploded")
	for range 2 {
		if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.probe.pids) != 2 {
		t.Fatalf("a probe that broke is tried again by the next load: %v", h.probe.pids)
	}
}

func TestARunWithNoProcessToMeasureIsMarked(t *testing.T) {
	h := newHarness(t, 0)
	h.b.opts.AttachPID = 0 // attach mode as serve wires it: there is no process to measure
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	h.b.mu.Lock()
	marked := h.b.noMemory["a"]
	h.b.mu.Unlock()
	if !marked || len(h.probe.pids) != 0 {
		t.Fatalf("no process to measure is final for the run: marked %v, probes %v", marked, h.probe.pids)
	}
	if err := h.b.Unload(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	h.b.mu.Lock()
	marked = h.b.noMemory["a"]
	h.b.mu.Unlock()
	if marked {
		t.Fatal("the mark goes with the run")
	}
}

func TestALoadReplacesTheFiguresOfTheRunBefore(t *testing.T) {
	h := newHarness(t, 0)
	ctx := context.Background()
	h.probe.err = measure.ErrNoFigure
	if err := h.b.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	h.probe.err, h.probe.mem = nil, backend.Memory{Bytes: 2 << 30, Source: "measured"}
	if err := h.b.Load(ctx, "a", backend.LoadOptions{Context: 8192}); err != nil {
		t.Fatal(err)
	}
	a := statusOf(t, h.b, "a")
	if a.Props == nil || a.Props.ContextLength != 8192 || a.Memory == nil || a.Memory.Bytes != 2<<30 {
		t.Fatalf("a load records the new run's props and memory, whatever the run before could give: %+v", a)
	}
}

func TestARecordingOfASleepingModelReadsItsPropsOnly(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.SetStatus("a", "sleeping")
	h.b.record(context.Background(), "a", true)
	a := statusOf(t, h.b, "a")
	if a.Props == nil || a.Memory != nil || len(h.probe.pids) != 0 {
		t.Fatalf("a model asleep keeps a fraction of a run's memory, so only its props are recorded: %+v, probes %v",
			a, h.probe.pids)
	}
}
