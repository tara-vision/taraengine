package backendtest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend"
)

//nolint:gocyclo // one linear scenario: every call checks what the calls before it recorded
func TestFakeRecordsCallsAndFails(t *testing.T) {
	f := New()
	ctx := context.Background()
	if err := f.Start(ctx); err != nil || !f.Started {
		t.Fatal("start")
	}
	f.SetModels(backend.ModelStatus{Name: "a", Status: backend.StatusPulled})
	if err := f.Load(ctx, "a", backend.LoadOptions{Context: 4096}); err != nil {
		t.Fatal(err)
	}
	models, _ := f.Models(ctx)
	if len(models) != 1 || models[0].Status != backend.StatusLoaded || models[0].Context != 4096 {
		t.Fatalf("load did not update the status: %+v", models)
	}
	if err := f.Unload(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	models, _ = f.Models(ctx)
	if models[0].Status != backend.StatusPulled {
		t.Fatalf("unload did not update the status: %+v", models)
	}
	if err := f.Load(ctx, "absent", backend.LoadOptions{}); !errors.Is(err, backend.ErrUnknownModel) {
		t.Fatalf("unknown model: %v", err)
	}
	if err := f.Refresh(ctx); err != nil || f.Refreshes != 1 {
		t.Fatal("refresh")
	}
	h, _ := f.Health(ctx)
	if !h.OK {
		t.Fatal("health")
	}
	f.Fail("load", errors.New("boom"))
	if err := f.Load(ctx, "a", backend.LoadOptions{}); err == nil || err.Error() != "boom" {
		t.Fatalf("injected failure: %v", err)
	}
	rec := httptest.NewRecorder()
	f.Proxy().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"proxied":true}` {
		t.Fatalf("proxy: %d %s", rec.Code, rec.Body.String())
	}
	if err := f.Stop(ctx); err != nil || f.Started {
		t.Fatal("stop")
	}
	if len(f.Loads) != 3 || f.Loads[0].Name != "a" || f.Loads[0].Options.Context != 4096 || len(f.Unloads) != 1 {
		t.Fatalf("recorded calls: %+v %+v", f.Loads, f.Unloads)
	}
}

// modelNamed returns the named model as the fake lists it.
func modelNamed(t *testing.T, f *Fake, name string) backend.ModelStatus {
	t.Helper()
	models, err := f.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("model %q is not listed", name)
	return backend.ModelStatus{}
}

func TestFakeDefaultLoadKeepsPresetContext(t *testing.T) {
	ctx := context.Background()
	f := New()
	f.SetModels(backend.ModelStatus{Name: "a", Status: backend.StatusPulled, Context: 8192})
	if err := f.Load(ctx, "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if m := modelNamed(t, f, "a"); m.Status != backend.StatusLoaded || m.Context != 8192 {
		t.Fatalf("default load: got %s at %d, want loaded at 8192", m.Status, m.Context)
	}
	if err := f.Unload(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if m := modelNamed(t, f, "a"); m.Status != backend.StatusPulled || m.Context != 8192 {
		t.Fatalf("unload: got %s at %d, want pulled at 8192", m.Status, m.Context)
	}
	if err := f.Load(ctx, "a", backend.LoadOptions{Context: 4096}); err != nil {
		t.Fatal(err)
	}
	if m := modelNamed(t, f, "a"); m.Context != 4096 {
		t.Fatalf("explicit load: got context %d, want 4096", m.Context)
	}
}

func TestFailRejectsUnknownOperation(t *testing.T) {
	var got any
	func() {
		defer func() { got = recover() }()
		New().Fail("lod", errors.New("boom"))
	}()
	msg, ok := got.(string)
	if !ok || !strings.Contains(msg, `"lod"`) || !strings.Contains(msg, "start, stop, models, load, unload, refresh, health") {
		t.Fatalf("misspelled operation: recovered %v, want a panic naming it and the valid operations", got)
	}
}

func TestFakeUnloadOfUnknownModelRecordsCallAndFails(t *testing.T) {
	f := New()
	if err := f.Unload(context.Background(), "absent"); !errors.Is(err, backend.ErrUnknownModel) {
		t.Fatalf("unload of an unknown model: %v, want ErrUnknownModel", err)
	}
	if len(f.Unloads) != 1 || f.Unloads[0] != "absent" {
		t.Fatalf("recorded unloads: %v, want the unknown name once", f.Unloads)
	}
}

func TestFakeRecordsEveryLoadOption(t *testing.T) {
	f := New()
	f.SetModels(backend.ModelStatus{Name: "a", Status: backend.StatusPulled})
	want := backend.LoadOptions{Context: 512, Draft: true, Force: true}
	if err := f.Load(context.Background(), "a", want); err != nil {
		t.Fatal(err)
	}
	if len(f.Loads) != 1 || f.Loads[0].Name != "a" || f.Loads[0].Options != want {
		t.Fatalf("recorded loads: %+v, want one load of a with %+v", f.Loads, want)
	}
}

func TestFakeInjectedFailuresReachEveryOperation(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("injected")
	calls := []struct {
		op   string
		call func(*Fake) error
	}{
		{"start", func(f *Fake) error { return f.Start(ctx) }},
		{"stop", func(f *Fake) error { return f.Stop(ctx) }},
		{"models", func(f *Fake) error { _, err := f.Models(ctx); return err }},
		{"load", func(f *Fake) error { return f.Load(ctx, "a", backend.LoadOptions{}) }},
		{"unload", func(f *Fake) error { return f.Unload(ctx, "a") }},
		{"refresh", func(f *Fake) error { return f.Refresh(ctx) }},
		{"health", func(f *Fake) error { _, err := f.Health(ctx); return err }},
	}
	for _, c := range calls {
		t.Run(c.op, func(t *testing.T) {
			f := New()
			f.Fail(c.op, boom)
			if err := c.call(f); !errors.Is(err, boom) {
				t.Fatalf("got %v, want the injected error", err)
			}
		})
	}
}
