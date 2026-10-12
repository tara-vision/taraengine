package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/backend/llamaserver"
	"github.com/tara-vision/taraengine/internal/catalog"
	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/gguf/gguftest"
	"github.com/tara-vision/taraengine/internal/llamatest"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/pkg/client"
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

// importedByAnOlderVersion puts a model file with a native window under the store's local links and indexes it
// without the window, as taraengine 0.1.0 did. It returns the index.
func importedByAnOlderVersion(t *testing.T, dataDir, name string, window uint32) *catalog.Index {
	t.Helper()
	store := pull.Store{Dir: filepath.Join(dataDir, "models")}
	dir := filepath.Join(store.Dir, "local")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	index := &catalog.Index{Path: filepath.Join(store.Dir, "index.json")}
	path := gguftest.WriteModel(t, dir, name+".gguf", "llama", window)
	if err := index.Add(catalog.IndexEntry{Name: name, Path: path, Size: 1}); err != nil {
		t.Fatal(err)
	}
	return index
}

// reloads counts the times the router was asked to re-read its presets.
func reloads(fake *llamatest.Server) int {
	n := 0
	for _, r := range fake.Requests() {
		if strings.HasPrefix(r, "GET /models?reload=1") {
			n++
		}
	}
	return n
}

func TestWireGivesAnOlderImportItsNativeWindow(t *testing.T) {
	dataDir := t.TempDir()
	index := importedByAnOlderVersion(t, dataDir, "short", 2048)
	wired, fake := wireAttached(t, dataDir, attachedConfig(dataDir), debugLogger(io.Discard))
	if entries, err := index.Load(); err != nil || len(entries) != 1 || entries[0].NativeContext != 2048 {
		t.Fatalf("the index keeps the window the header gave: %+v %v", entries, err)
	}
	presets, err := os.ReadFile(llamaserver.PresetPath(dataDir))
	if err != nil || !strings.Contains(string(presets), "[short]\nc = 2048\n") || reloads(fake) != 2 {
		t.Fatalf("the router re-read presets that carry the window (%d reloads): %v\n%s", reloads(fake), err, presets)
	}
	srv := httptest.NewServer(wired.Handler)
	t.Cleanup(srv.Close)
	models, err := client.New(srv.URL, "").Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.Name == "short" && (m.NativeContext != 2048 || m.Context != 2048) {
			t.Fatalf("short: %+v", m)
		}
	}
}

func TestWireStartsWhateverTheImportsHold(t *testing.T) {
	dataDir := t.TempDir()
	index := &catalog.Index{Path: filepath.Join(dataDir, "models", "index.json")}
	plain := filepath.Join(t.TempDir(), "plain.gguf")
	writeText(t, plain, "x")
	for _, e := range []catalog.IndexEntry{
		{Name: "plain", Path: plain, Size: 1},
		{Name: "gone", Path: filepath.Join(t.TempDir(), "gone.gguf"), Size: 1},
	} {
		if err := index.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	var logs syncBuffer
	_, fake := wireAttached(t, dataDir, attachedConfig(dataDir), debugLogger(&logs))
	out := logs.String()
	if !strings.Contains(out, "the native context window of an import is unknown") || !strings.Contains(out, "model=plain") ||
		strings.Contains(out, "model=gone") {
		t.Fatalf("an unreadable import is logged, one whose file is gone is not:\n%s", out)
	}
	if n := reloads(fake); n != 1 {
		t.Fatalf("with no window found the presets are not rendered again: %d reloads", n)
	}
}

func TestWireStartsWhenTheWindowsCannotBeSaved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	dataDir := t.TempDir()
	index := importedByAnOlderVersion(t, dataDir, "short", 2048)
	models := filepath.Dir(index.Path)
	if err := os.Chmod(models, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(models, 0o750) })
	var logs syncBuffer
	_, fake := wireAttached(t, dataDir, attachedConfig(dataDir), debugLogger(&logs))
	if !strings.Contains(logs.String(), "could not save the native context windows of the imports") || reloads(fake) != 1 {
		t.Fatalf("a save that fails is logged and nothing is rendered again (%d reloads):\n%s", reloads(fake), logs.String())
	}
	if entries, err := index.Load(); err != nil || entries[0].NativeContext != 0 {
		t.Fatalf("the window stays unknown until the next start: %+v %v", entries, err)
	}
}

func TestWireStartsWhenTheRouterDoesNotPickUpTheWindows(t *testing.T) {
	dataDir := t.TempDir()
	index := importedByAnOlderVersion(t, dataDir, "short", 2048)
	fake := llamatest.New(t, llamatest.Options{APIKey: "k", PresetPath: llamaserver.PresetPath(dataDir)})
	target, err := url.Parse(fake.URL())
	if err != nil {
		t.Fatal(err)
	}
	pass := httputil.NewSingleHostReverseProxy(target)
	var seen atomic.Int32
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("reload") == "1" && seen.Add(1) == 2 { // the start's reload passes, the backfill's does not
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":500,"message":"scripted failure"}}`))
			return
		}
		pass.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	var logs syncBuffer
	wired, err := Wire(context.Background(), attachedConfig(dataDir),
		WireOptions{Attach: front.URL, AttachKey: "k", Logger: debugLogger(&logs)})
	if err != nil {
		t.Fatalf("the daemon starts anyway: %v", err)
	}
	t.Cleanup(func() { _ = wired.Backend.Stop(context.Background()) })
	if !strings.Contains(logs.String(), "the router did not pick up the native context windows of the imports") {
		t.Fatalf("the refused reload is logged:\n%s", logs.String())
	}
	if entries, err := index.Load(); err != nil || entries[0].NativeContext != 2048 {
		t.Fatalf("the window is saved all the same, for the next render: %+v %v", entries, err)
	}
}

func TestWireSavesTheCapabilitiesAnImportShowsAndShowsThemAfterARestart(t *testing.T) {
	dataDir := t.TempDir()
	index := importedByAnOlderVersion(t, dataDir, "short", 2048)
	writeFiles(t, pull.Store{Dir: filepath.Join(dataDir, "models")}, "org/tiny", "tiny.gguf")
	if err := os.WriteFile(filepath.Join(dataDir, "registry.yaml"), []byte(tinyRegistry), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	wired, _ := wireAttached(t, dataDir, attachedConfig(dataDir), debugLogger(io.Discard))
	for _, name := range []string{"short", "tiny"} {
		if err := wired.Backend.Load(ctx, name, backend.LoadOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := index.Load()
	if err != nil || len(entries) != 1 || !slices.Equal(entries[0].Capabilities, []string{"tools", "thinking"}) {
		t.Fatalf("the import's capabilities are saved, and the registry model gets no entry: %+v %v", entries, err)
	}
	if err := wired.Backend.Unload(ctx, "short"); err != nil {
		t.Fatal(err)
	}
	if err := wired.Backend.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	again, _ := wireAttached(t, dataDir, attachedConfig(dataDir), debugLogger(io.Discard))
	srv := httptest.NewServer(again.Handler)
	t.Cleanup(srv.Close)
	models, err := client.New(srv.URL, "").Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.Name == "short" && (m.Status != "pulled" || strings.Join(m.Capabilities, ",") != "tools,thinking") {
			t.Fatalf("after an unload and a restart the import still shows what it can do: %+v", m)
		}
	}
}

func TestSaveCapabilitiesLogsAnIndexItCannotWrite(t *testing.T) {
	index := &catalog.Index{Path: filepath.Join(t.TempDir(), "index.json")}
	if err := os.WriteFile(index.Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	saveCapabilities(index, debugLogger(&logs))("mine", backend.Props{SupportsTools: true})
	if !strings.Contains(logs.String(), "could not save the capabilities of an import") {
		t.Fatalf("a save that fails is logged:\n%s", logs.String())
	}
}
