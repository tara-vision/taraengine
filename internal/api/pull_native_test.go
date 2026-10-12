package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/gguf/gguftest"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/pkg/api"
)

func TestAnImportTakesItsNativeWindowFromItsFile(t *testing.T) {
	h := newHarness(t, "")
	model := gguftest.Model("llama", 2048)
	h.fetcher.artifacts["org/repo/tiny.gguf"] = pull.Artifact{Repo: "org/repo", File: "tiny.gguf", Size: int64(len(model)), SHA256: "abc"}
	h.fetcher.bodies = map[string][]byte{"tiny.gguf": model}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "hf://org/repo/tiny.gguf"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("hf pull: %d", resp.StatusCode)
	}
	local := gguftest.WriteModel(t, t.TempDir(), "mine.gguf", "gemma3", 4096)
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: local}); resp.StatusCode != http.StatusOK {
		t.Fatalf("local pull: %d", resp.StatusCode)
	}
	entries, err := h.cat.Index.Load()
	if err != nil || len(entries) != 2 || entries[0].NativeContext != 4096 || entries[1].NativeContext != 2048 {
		t.Fatalf("the index keeps the window each header gave: %+v %v", entries, err)
	}
	models := modelsByName(t, h)
	if m := models["tiny"]; m.NativeContext != 2048 || m.Context != 2048 {
		t.Fatalf("an import runs at most at the window it was trained for: %+v", m)
	}
	if m := models["mine"]; m.NativeContext != 4096 || m.Context != 4096 {
		t.Fatalf("mine: %+v", m)
	}
}

func TestAnImportWhoseHeaderCannotBeReadIsPulledAllTheSame(t *testing.T) {
	h := newHarness(t, "")
	local := filepath.Join(t.TempDir(), "Plain.gguf")
	if err := os.WriteFile(local, []byte("not a model header"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: local}); resp.StatusCode != http.StatusOK {
		t.Fatalf("a file without a readable header still pulls: %d", resp.StatusCode)
	}
	if m := modelsByName(t, h)["Plain"]; m.Status != "pulled" || m.NativeContext != 0 || m.Context != h.deps.Config.Context {
		t.Fatalf("its window stays unknown: %+v", m)
	}
	if !strings.Contains(h.logs.String(), "the native context window of the model is unknown") {
		t.Fatalf("the log says why:\n%s", h.logs.String())
	}
}
