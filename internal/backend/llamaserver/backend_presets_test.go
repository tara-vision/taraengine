package llamaserver

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/llamatest"
)

func readPresets(t *testing.T, h *harness) string {
	t.Helper()
	data, err := os.ReadFile(PresetPath(h.b.opts.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func withExtra(m PresetModel, key, value string) PresetModel {
	m.Extra = map[string]string{key: value}
	return m
}

func withModel(name string) func(h *harness) {
	return func(h *harness) {
		h.models[name] = PresetModel{Name: name, ModelPath: "/m/" + name + ".gguf"}
		h.order = append(h.order, name)
	}
}

func TestRefreshWritesTheCatalogAndTheRouterReloadsIt(t *testing.T) {
	h := newHarness(t, 0)
	withModel("c")(h)
	if err := h.b.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readPresets(t, h), "[c]\nmodel = /m/c.gguf\n") {
		t.Fatalf("presets after a refresh:\n%s", readPresets(t, h))
	}
	if c := statusOf(t, h.b, "c"); c.Status != backend.StatusPulled {
		t.Fatalf("c: %+v", c)
	}
	h.order = []string{"a"}
	if err := h.b.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	models, _ := h.b.Models(context.Background())
	if len(models) != 1 || models[0].Name != "a" {
		t.Fatalf("a removed model must leave the list: %+v", models)
	}
	if err := h.b.Load(context.Background(), "b", backend.LoadOptions{}); !errors.Is(err, backend.ErrUnknownModel) {
		t.Fatalf("a removed model cannot be loaded: %v", err)
	}
	if err := h.b.Unload(context.Background(), "b"); !errors.Is(err, backend.ErrUnknownModel) {
		t.Fatalf("a removed model cannot be unloaded: %v", err)
	}
}

func TestRefreshKeepsThePersistedContextOverrides(t *testing.T) {
	h := newHarness(t, 0)
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{Context: 8192}); err != nil {
		t.Fatal(err)
	}
	if err := h.b.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readPresets(t, h), "[a]\nc = 8192\n") {
		t.Fatalf("a refresh must render the override again:\n%s", readPresets(t, h))
	}
}

func TestRefreshLeavesOutALaterNameThatSharesARouterID(t *testing.T) {
	h := newHarness(t, 0)
	rec := &logRecorder{}
	h.b.opts.Logger = slog.New(rec)
	withModel("x-y")(h)
	withModel("x:y")(h)
	withModel("c")(h)
	if err := h.b.Refresh(context.Background()); err != nil {
		t.Fatalf("a clash must never stop the refresh: %v", err)
	}
	presets := readPresets(t, h)
	if strings.Count(presets, "[x-y]") != 1 || !strings.Contains(presets, "[x-y]\nmodel = /m/x-y.gguf\n") ||
		!strings.Contains(presets, "[c]\n") {
		t.Fatalf("the name listed first keeps the id, the other is not rendered:\n%s", presets)
	}
	names := []string{}
	models, _ := h.b.Models(context.Background())
	for _, m := range models {
		names = append(names, m.Name)
	}
	if slices.Sort(names); !slices.Equal(names, []string{"a", "b", "c", "x-y"}) {
		t.Fatalf("roster %v", names)
	}
	if err := h.b.Load(context.Background(), "x:y", backend.LoadOptions{}); !errors.Is(err, backend.ErrUnknownModel) {
		t.Fatalf("the name left out is unknown: %v", err)
	}
	if !rec.Contains("x:y") || !rec.Contains("x-y") || rec.Count("model left out") != 1 {
		t.Fatalf("the clash is logged once, naming both: %v", rec.lines)
	}
}

func TestStartServesTheCatalogWhenTwoNamesShareARouterID(t *testing.T) {
	h := newHarnessWith(t, 0, func(_ *llamatest.Server, o *Options) {
		catalog := o.Models
		o.Models = func(ctx context.Context, overrides map[string]int) ([]PresetModel, error) {
			models, err := catalog(ctx, overrides)
			return append(models, PresetModel{Name: "a:1", ModelPath: "/m/a1.gguf"},
				PresetModel{Name: "a-1", ModelPath: "/m/escape.gguf"}), err
		}
	})
	if m := statusOf(t, h.b, "a:1"); m.Status != backend.StatusPulled {
		t.Fatalf("the name listed first is served: %+v", m)
	}
	if strings.Contains(readPresets(t, h), "escape.gguf") {
		t.Fatalf("the later name must not reach the preset file:\n%s", readPresets(t, h))
	}
}

func TestRefreshRefusesPresetsTheIniFormatCannotHold(t *testing.T) {
	rows := []struct {
		name   string
		mutate func(h *harness)
		want   []string
	}{
		{"line feed in an extra value", func(h *harness) { h.models["a"] = withExtra(h.models["a"], "cache-type-k", "q8\nmodel = /evil") }, []string{`"a"`, "cache-type-k"}},
		{"carriage return in an extra value", func(h *harness) { h.models["a"] = withExtra(h.models["a"], "cache-type-k", "q8\rx") }, []string{`"a"`, "cache-type-k"}},
		{"NUL in an extra value", func(h *harness) { h.models["a"] = withExtra(h.models["a"], "cache-type-k", "q8\x00") }, []string{`"a"`, "cache-type-k"}},
		{"line feed in a key", func(h *harness) { h.models["a"] = withExtra(h.models["a"], "k\nmodel", "x") }, []string{`"a"`, "model"}},
		{"equals sign in a key", func(h *harness) { h.models["a"] = withExtra(h.models["a"], "a=b", "x") }, []string{`"a"`, "a=b"}},
		{"key opening a section", func(h *harness) { h.models["a"] = withExtra(h.models["a"], "[evil", "x") }, []string{`"a"`, "[evil"}},
		{"key opening a comment", func(h *harness) { h.models["a"] = withExtra(h.models["a"], "#evil", "x") }, []string{`"a"`, "#evil"}},
		{"key opening a semicolon comment", func(h *harness) { h.models["a"] = withExtra(h.models["a"], ";evil", "x") }, []string{`"a"`, ";evil"}},
		{"empty key", func(h *harness) { h.models["a"] = withExtra(h.models["a"], "", "x") }, []string{`"a"`, "empty"}},
		{"key with leading space", func(h *harness) { h.models["a"] = withExtra(h.models["a"], " model", "/evil") }, []string{`"a"`, "whitespace"}},
		{"key with trailing space", func(h *harness) { h.models["a"] = withExtra(h.models["a"], "model ", "/evil") }, []string{`"a"`, "whitespace"}},
		{"line feed in the model path", func(h *harness) {
			m := h.models["a"]
			m.ModelPath = "/m/a\n.gguf"
			h.models["a"] = m
		}, []string{`"a"`, "model"}},
		{"carriage return in the projector path", func(h *harness) {
			m := h.models["a"]
			m.MMProjPath = "/m/p\r.gguf"
			h.models["a"] = m
		}, []string{`"a"`, "mmproj"}},
		{"NUL in the draft path", func(h *harness) {
			m := h.models["a"]
			m.DraftPath = "/m/d\x00.gguf"
			h.models["a"] = m
		}, []string{`"a"`, "model-draft"}},
		{"line feed in a global value", func(h *harness) { h.b.opts.Global = map[string]string{"jinja": "true\nmodel = /evil"} }, []string{"global", "jinja"}},
		{"equals sign in a global key", func(h *harness) { h.b.opts.Global = map[string]string{"a=b": "x"} }, []string{"global", "a=b"}},
		{"section name closing the header", withModel("a]b"), []string{"section name"}},
		{"section name opening a header", withModel("a[b"), []string{"section name"}},
		{"section name with a line feed", withModel("a\nb"), []string{"section name"}},
		{"empty section name", withModel(""), []string{"section name"}},
		{"the global section as a name", withModel("*"), []string{"section name"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := newHarness(t, 0)
			before := readPresets(t, h)
			row.mutate(h)
			err := h.b.Refresh(context.Background())
			if err == nil {
				t.Fatal("the refresh was accepted")
			}
			for _, fragment := range row.want {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("error %q lacks %q", err, fragment)
				}
			}
			if strings.ContainsAny(err.Error(), "\r\n\x00") {
				t.Errorf("the error must not carry raw line breaks or NUL from the offending input: %q", err)
			}
			if readPresets(t, h) != before {
				t.Error("a refused refresh must leave the preset file alone")
			}
		})
	}
}

func TestRefreshReportsTheCatalogAndTheRouter(t *testing.T) {
	h := newHarness(t, 0)
	broken := errors.New("catalog unreadable")
	good := h.b.opts.Models
	h.b.opts.Models = func(context.Context, map[string]int) ([]PresetModel, error) { return nil, broken }
	if err := h.b.Refresh(context.Background()); !errors.Is(err, broken) {
		t.Fatalf("a catalog failure must reach the caller: %v", err)
	}
	h.b.opts.Models = good
	presets := PresetPath(h.b.opts.DataDir)
	if err := os.Remove(presets); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(presets, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := h.b.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "write presets") {
		t.Fatalf("a preset file that cannot be written must be reported: %v", err)
	}
	if err := os.Remove(presets); err != nil {
		t.Fatal(err)
	}
	h.fake.Close()
	if err := h.b.Refresh(context.Background()); err == nil {
		t.Fatal("a refresh the router cannot answer must fail")
	}
}
