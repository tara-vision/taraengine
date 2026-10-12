package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/registry"
	"github.com/tara-vision/taraengine/pkg/api"
)

func modelsByName(t *testing.T, h *harness) map[string]api.Model {
	t.Helper()
	_, data := h.do(t, http.MethodGet, "/api/models", "", nil)
	byName := map[string]api.Model{}
	for _, m := range decode[[]api.Model](t, data) {
		byName[m.Name] = m
	}
	return byName
}

func TestModelsListMergesCatalogAndBackend(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusLoaded, Context: 32768, Memory: &backend.Memory{Bytes: 9 << 30, Source: "measured"}})
	_, data := h.do(t, http.MethodGet, "/api/models", "", nil)
	models := decode[[]api.Model](t, data)
	if len(models) != 8 {
		t.Fatalf("%d models", len(models))
	}
	byName := map[string]api.Model{}
	for _, m := range models {
		byName[m.Name] = m
	}
	g := byName["gemma4:12b"]
	if g.Status != "loaded" || g.Context != 32768 || g.Memory == nil || g.Memory.Bytes != 9<<30 || g.Tier != "16" || !g.Default || g.Size != 7219673216 {
		t.Fatalf("gemma: %+v", g)
	}
	q := byName["qwen3.6:35b"]
	if q.Status != "available" || q.Context != 32768 || q.NativeContext != 262144 || len(q.Capabilities) != 3 || q.CapabilitiesInferred {
		t.Fatalf("qwen: %+v", q)
	}
	_, data = h.do(t, http.MethodGet, "/api/loaded", "", nil)
	loaded := decode[[]api.Loaded](t, data)
	if len(loaded) != 1 || loaded[0].Name != "gemma4:12b" || loaded[0].Memory == nil {
		t.Fatalf("loaded: %+v", loaded)
	}
}

func TestModelsListCarriesTheRegistryFields(t *testing.T) {
	h := newHarness(t, "")
	models := modelsByName(t, h)
	g := models["glm-4.7-flash"]
	if g.Name != "glm-4.7-flash" || g.Status != "available" || g.Size != 18244193920 || g.Family != "glm-4.7-flash" ||
		g.Params != "30B-A3B" || g.Quantization != "Q4_K" || g.Context != 32768 || g.NativeContext != 198000 ||
		strings.Join(g.Capabilities, ",") != "tools,thinking" || g.Tier != "32" || !g.Default || g.Memory != nil {
		t.Fatalf("glm: %+v", g)
	}
	if other := models["gemma4:26b"]; other.Default {
		t.Fatalf("a model that is no tier default must not say so: %+v", other)
	}
	if small := models["gemma4:e4b"]; small.Context != 32768 || small.Tier != "small" || !small.Default {
		t.Fatalf("e4b: %+v", small)
	}
}

func TestContextFollowsTheConfigAndNeverExceedsTheNativeWindow(t *testing.T) {
	h := newHarness(t, "", func(d *Deps) {
		d.Config.ContextByTier = map[string]int{"16": 8192, "48": 1 << 20, "other": 4096}
	})
	addHFModel(t, h, "extra", "org/repo", "extra.gguf")
	models := modelsByName(t, h)
	if got := models["gemma4:12b"].Context; got != 8192 {
		t.Errorf("tier 16 window %d, want 8192", got)
	}
	if got := models["qwen3.6:35b"].Context; got != 262144 {
		t.Errorf("a tier value above the model's native window must be capped at it: %d", got)
	}
	if got := models["extra"].Context; got != 4096 {
		t.Errorf("a model outside the registry takes the other tier: %d", got)
	}
}

func TestModelStatusDetails(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	pulled(t, h, "glm-4.7-flash")
	h.fake.SetModels(
		backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusFailed, Failed: true, ExitCode: 137, LogTail: "CUDA out of memory"},
		backend.ModelStatus{Name: "glm-4.7-flash", Status: backend.StatusFaulty},
		backend.ModelStatus{Name: "qwen3.6:35b", Status: backend.StatusLoaded, Context: 4096},
	)
	models := modelsByName(t, h)
	g := models["gemma4:12b"]
	if g.Status != "failed" || g.ExitCode != 137 || g.Error != "the last load exited with exit code 137" || g.Memory != nil || g.Context != 32768 {
		t.Fatalf("failed model: %+v", g)
	}
	if f := models["glm-4.7-flash"]; f.Status != "faulty" || f.ExitCode != 0 || f.Error != "" || f.Context != 32768 {
		t.Fatalf("faulty model: %+v", f)
	}
	if q := models["qwen3.6:35b"]; q.Status != "available" || q.Context != 32768 {
		t.Fatalf("the backend's word about a model that is not pulled must not count: %+v", q)
	}
	_, data := h.do(t, http.MethodGet, "/api/models/gemma4:12b", "", nil)
	if d := decode[api.ModelDetail](t, data); d.LogTail != "CUDA out of memory" || d.ExitCode != 137 {
		t.Fatalf("show of a failed model: %+v", d)
	}
	_, data = h.do(t, http.MethodGet, "/api/models/glm-4.7-flash", "", nil)
	if d := decode[api.ModelDetail](t, data); d.LogTail != "" {
		t.Fatalf("log tail: %+v", d)
	}
}

func TestLoadedListsLoadedAndSleepingModelsOnly(t *testing.T) {
	h := newHarness(t, "")
	for _, name := range []string{"gemma4:12b", "glm-4.7-flash", "gemma4:e4b", "gemma4:26b"} {
		pulled(t, h, name)
	}
	h.fake.SetModels(
		backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusSleeping, Context: 4096},
		backend.ModelStatus{Name: "glm-4.7-flash", Status: backend.StatusLoading},
		backend.ModelStatus{Name: "gemma4:e4b", Status: backend.StatusPulled},
		backend.ModelStatus{Name: "gemma4:26b", Status: backend.StatusLoaded, Context: 8192, Memory: &backend.Memory{Bytes: 5, Source: "estimated"}},
	)
	_, data := h.do(t, http.MethodGet, "/api/loaded", "", nil)
	loaded := decode[[]api.Loaded](t, data)
	if len(loaded) != 2 {
		t.Fatalf("loaded: %+v", loaded)
	}
	byName := map[string]api.Loaded{}
	for _, l := range loaded {
		byName[l.Name] = l
	}
	if s := byName["gemma4:12b"]; s.Context != 4096 || s.Memory != nil || s.Status != "sleeping" {
		t.Fatalf("sleeping model: %+v", s)
	}
	if l := byName["gemma4:26b"]; l.Context != 8192 || l.Memory == nil || l.Memory.Bytes != 5 ||
		l.Memory.Source != "estimated" || l.Status != "loaded" {
		t.Fatalf("loaded model: %+v", l)
	}
	h.fake.SetModels()
	if _, data = h.do(t, http.MethodGet, "/api/loaded", "", nil); strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("nothing loaded must be an empty list, not null: %s", data)
	}
}

func TestEscapeHatchCapabilitiesAreInferredFromTheRunningModel(t *testing.T) {
	h := newHarness(t, "")
	addHFModel(t, h, "extra", "org/repo", "extra.gguf")
	pulled(t, h, "gemma4:12b")
	h.fake.SetModels(
		backend.ModelStatus{Name: "extra", Status: backend.StatusLoaded, Props: &backend.Props{SupportsTools: true, Vision: true}},
		backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusLoaded, Props: &backend.Props{}},
	)
	models := modelsByName(t, h)
	if e := models["extra"]; !e.CapabilitiesInferred || strings.Join(e.Capabilities, ",") != "tools,vision" {
		t.Fatalf("extra: %+v", e)
	}
	if g := models["gemma4:12b"]; g.CapabilitiesInferred || len(g.Capabilities) != 3 {
		t.Fatalf("a curated model keeps its capabilities whatever the props say: %+v", g)
	}
	h.fake.SetModels(backend.ModelStatus{Name: "extra", Status: backend.StatusLoaded, Props: &backend.Props{Thinking: true}})
	if e := modelsByName(t, h)["extra"]; strings.Join(e.Capabilities, ",") != "thinking" {
		t.Fatalf("thinking: %+v", e)
	}
	h.fake.SetModels(backend.ModelStatus{Name: "extra", Status: backend.StatusPulled})
	status, data := h.doRaw(t, http.MethodGet, "/api/models/extra", "", "")
	if status != 200 || !strings.Contains(string(data), `"capabilities":[]`) || !strings.Contains(string(data), `"capabilities_inferred":true`) {
		t.Fatalf("a model nothing is known about shows no capabilities, as an empty list: %d %s", status, data)
	}
}

func TestModelsShowRejectsBadName(t *testing.T) {
	h := newHarness(t, "")
	for _, name := range []string{"..%2Fetc", "a%2Fb", strings.Repeat("x", 70), "-x"} {
		if resp, _ := h.do(t, http.MethodGet, "/api/models/"+name, "", nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: %d", name, resp.StatusCode)
		}
	}
	if resp, _ := h.do(t, http.MethodGet, "/api/models/absent", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("absent: %d", resp.StatusCode)
	}
	_, data := h.do(t, http.MethodGet, "/api/models/gemma4:12b", "", nil)
	d := decode[api.ModelDetail](t, data)
	if d.Name != "gemma4:12b" || d.Status != "available" || d.Digest != "3712b9bd32ca" || d.Preset["c"] != "32768" {
		t.Fatalf("detail: %+v", d)
	}
}

func TestShowMergesTheRegistryPresetAndTheContextInEffect(t *testing.T) {
	reg := `
entries:
  - name: tuned:1b
    family: tuned
    params: 1B
    hf_repo: org/tuned
    file: tuned.gguf
    sha256: ` + strings.Repeat("a", 64) + `
    size: 10
    quantization: Q4_0
    context: 16384
    capabilities: [tools]
    tier: small
    preset:
      flash-attn: "on"
      cache-type-k: q8_0
`
	h := newHarness(t, "", func(d *Deps) {
		parsed, err := registry.Parse([]byte(reg))
		if err != nil {
			t.Fatal(err)
		}
		d.Catalog.Registry = parsed
	})
	_, data := h.do(t, http.MethodGet, "/api/models/tuned:1b", "", nil)
	d := decode[api.ModelDetail](t, data)
	if d.Preset["flash-attn"] != "on" || d.Preset["cache-type-k"] != "q8_0" || d.Preset["c"] != "16384" || len(d.Preset) != 3 {
		t.Fatalf("preset: %v", d.Preset)
	}
	if d.Digest != strings.Repeat("a", 12) {
		t.Fatalf("digest %q", d.Digest)
	}
}

func TestShowOfAnEscapeHatchModel(t *testing.T) {
	h := newHarness(t, "")
	addHFModel(t, h, "extra", "org/repo", "extra.gguf")
	_, data := h.do(t, http.MethodGet, "/api/models/extra", "", nil)
	d := decode[api.ModelDetail](t, data)
	if d.Name != "extra" || d.Status != "pulled" || d.Digest != "" || d.Tier != "" || len(d.Preset) != 1 || d.Preset["c"] != "32768" || d.Size != 1 {
		t.Fatalf("detail: %+v", d)
	}
}
