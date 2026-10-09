package llamaserver

import (
	"context"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend"
)

func TestModelsForgetFiguresOfAModelTheRouterNoLongerRuns(t *testing.T) {
	h := newHarness(t, 0)
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	h.fake.SetStatus("a", "unloaded") // an eviction, or a reload that unloaded an edited section
	if a := statusOf(t, h.b, "a"); a.Memory != nil || a.Props != nil {
		t.Fatalf("a model that is not running shows no figures: %+v", a)
	}
	h.fake.SetStatus("a", "loaded") // the router brings it back on its own, as an autoload does
	if a := statusOf(t, h.b, "a"); a.Status != backend.StatusLoaded || a.Memory != nil || a.Props != nil {
		t.Fatalf("figures of the earlier run must not come back with the model: %+v", a)
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
