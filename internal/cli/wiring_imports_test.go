package cli

import (
	"context"
	"testing"

	"github.com/tara-vision/taraengine/internal/catalog"
	"github.com/tara-vision/taraengine/internal/config"
)

func TestPresetModelsCapAnImportAtItsNativeWindow(t *testing.T) {
	cat, _, local := tinyCatalog(t)
	if err := cat.Index.Add(catalog.IndexEntry{Name: "short", Path: local, Size: 1, NativeContext: 2048}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	models, err := presetModels(cat, cfg)(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	windows := map[string]int{}
	for _, m := range models {
		windows[m.Name] = m.Context
	}
	if windows["short"] != 2048 || windows["mine"] != cfg.Context || windows["tiny"] != 512 {
		t.Fatalf("an import runs at most at its native window, one with none known at the default: %v", windows)
	}
}
