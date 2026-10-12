package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend/llamaserver"
	"github.com/tara-vision/taraengine/internal/catalog"
	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/llamatest"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
	"github.com/tara-vision/taraengine/internal/runtime"
	"github.com/tara-vision/taraengine/internal/version"
	"github.com/tara-vision/taraengine/pkg/client"
)

// syncBuffer is a log sink that can be read while a daemon is still writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func debugLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// tinyRegistry has a model with every kind of file and a tuning key, and one that is never pulled.
var tinyRegistry = `entries:
  - name: tiny
    hf_repo: org/tiny
    file: tiny.gguf
    sha256: ` + strings.Repeat("a", 64) + `
    size: 10
    context: 512
    tier: small
    default: true
    preset:
      cache-type-k: q8_0
    mmproj:
      file: tiny-mmproj.gguf
      sha256: ` + strings.Repeat("b", 64) + `
      size: 5
    mtp:
      file: tiny-mtp.gguf
      sha256: ` + strings.Repeat("c", 64) + `
      size: 5
  - name: big
    hf_repo: org/big
    file: big.gguf
    sha256: ` + strings.Repeat("d", 64) + `
    size: 10
    context: 8192
    tier: "16"
`

// writeFiles creates the named files under the store, so that the catalog sees them as pulled.
func writeFiles(t *testing.T, store pull.Store, repo string, files ...string) {
	t.Helper()
	for _, f := range files {
		path := store.Path(repo, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// attachedConfig is a configuration for a data directory of its own and a router at the fake's address.
func attachedConfig(dataDir string) config.Config {
	cfg := config.Default()
	cfg.DataDir = dataDir
	return cfg
}

// wireAttached wires a daemon to a fake router and returns it with its parts.
func wireAttached(t *testing.T, dataDir string, cfg config.Config, logger *slog.Logger) (*Wired, *llamatest.Server) {
	t.Helper()
	fake := llamatest.New(t, llamatest.Options{APIKey: "k", PresetPath: llamaserver.PresetPath(dataDir)})
	wired, err := Wire(context.Background(), cfg, WireOptions{Attach: fake.URL(), AttachKey: "k", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wired.Backend.Stop(context.Background()) })
	return wired, fake
}

// tinyCatalog is a catalog of the tiny registry with the model "tiny" fully pulled, "big" not pulled and a local
// model "mine" in the index.
func tinyCatalog(t *testing.T) (catalog.Catalog, pull.Store, string) {
	t.Helper()
	dir := t.TempDir()
	reg, err := registry.Parse([]byte(tinyRegistry))
	if err != nil {
		t.Fatal(err)
	}
	store := pull.Store{Dir: filepath.Join(dir, "models")}
	index := &catalog.Index{Path: filepath.Join(store.Dir, "index.json")}
	writeFiles(t, store, "org/tiny", "tiny.gguf", "tiny-mmproj.gguf", "tiny-mtp.gguf")
	local := filepath.Join(dir, "mine.gguf")
	writeText(t, local, "x")
	if err := index.Add(catalog.IndexEntry{Name: "mine", Path: local, Size: 1}); err != nil {
		t.Fatal(err)
	}
	return catalog.Catalog{Registry: reg, Store: store, Index: index}, store, local
}

func TestPresetModelsListsThePulledModelsWithTheirWindowAndTuning(t *testing.T) {
	cat, store, local := tinyCatalog(t)
	cfg := config.Default()
	cfg.Context = 4096
	cfg.ContextByTier = map[string]int{"other": 2048}

	models, err := presetModels(cat, cfg)(context.Background(), nil)
	if err != nil || len(models) != 2 {
		t.Fatalf("only pulled models are listed: %+v %v", models, err)
	}
	tiny, mine := models[0], models[1]
	if tiny.Name != "tiny" || tiny.ModelPath != store.Path("org/tiny", "tiny.gguf") ||
		tiny.MMProjPath != store.Path("org/tiny", "tiny-mmproj.gguf") || tiny.DraftPath != store.Path("org/tiny", "tiny-mtp.gguf") {
		t.Fatalf("tiny: %+v", tiny)
	}
	if tiny.Context != 512 || len(tiny.Extra) != 1 || tiny.Extra["cache-type-k"] != "q8_0" {
		t.Fatalf("the window never exceeds the model's own, and the registry's tuning keys come along: %+v", tiny)
	}
	if mine.Name != "mine" || mine.ModelPath != local || mine.Context != 2048 || len(mine.Extra) != 0 || mine.MMProjPath != "" || mine.DraftPath != "" {
		t.Fatalf("a model from outside the registry takes the other tier: %+v", mine)
	}
}

func TestPresetModelsTakeTheWindowTheStateRemembers(t *testing.T) {
	cat, _, _ := tinyCatalog(t)
	models, err := presetModels(cat, config.Default())(context.Background(), map[string]int{"tiny": 1024, "mine": 1536})
	if err != nil || len(models) != 2 || models[0].Context != 1024 || models[1].Context != 1536 {
		t.Fatalf("%+v %v", models, err)
	}
}

func TestPresetModelsLeavesOutASiblingThatIsNotPulled(t *testing.T) {
	dir := t.TempDir()
	reg, _ := registry.Parse([]byte(tinyRegistry))
	store := pull.Store{Dir: filepath.Join(dir, "models")}
	cat := catalog.Catalog{Registry: reg, Store: store, Index: &catalog.Index{Path: filepath.Join(store.Dir, "index.json")}}
	writeFiles(t, store, "org/tiny", "tiny.gguf", "tiny-mtp.gguf")
	models, err := presetModels(cat, config.Default())(context.Background(), nil)
	if err != nil || len(models) != 1 || models[0].MMProjPath != "" || models[0].DraftPath == "" {
		t.Fatalf("the draft is part of the preset once it is pulled, the projector is not before: %+v %v", models, err)
	}
}

func TestPresetModelsNeverShareTheRegistrysMaps(t *testing.T) {
	dir := t.TempDir()
	reg, _ := registry.Parse([]byte(tinyRegistry))
	store := pull.Store{Dir: filepath.Join(dir, "models")}
	cat := catalog.Catalog{Registry: reg, Store: store, Index: &catalog.Index{Path: filepath.Join(store.Dir, "index.json")}}
	writeFiles(t, store, "org/tiny", "tiny.gguf")
	list := presetModels(cat, config.Default())
	first, _ := list(context.Background(), nil)
	first[0].Extra["injected"] = "yes"
	second, _ := list(context.Background(), nil)
	if _, leaked := second[0].Extra["injected"]; leaked {
		t.Fatal("a change to one list reached the next")
	}
	if _, leaked := reg.Entries[0].Preset["injected"]; leaked {
		t.Fatal("a change to a list reached the registry")
	}
}

func TestPresetModelsReportsABrokenIndex(t *testing.T) {
	dir := t.TempDir()
	reg, _ := registry.Parse([]byte(tinyRegistry))
	store := pull.Store{Dir: filepath.Join(dir, "models")}
	index := &catalog.Index{Path: filepath.Join(store.Dir, "index.json")}
	if err := os.MkdirAll(store.Dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index.Path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cat := catalog.Catalog{Registry: reg, Store: store, Index: index}
	if _, err := presetModels(cat, config.Default())(context.Background(), nil); err == nil {
		t.Fatal("a broken index must not read as an empty one")
	}
}

func TestWireWritesThePresetsAndServesThePulledModels(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "registry.yaml"), []byte(tinyRegistry), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, pull.Store{Dir: filepath.Join(dataDir, "models")}, "org/tiny", "tiny.gguf", "tiny-mtp.gguf")
	cfg := attachedConfig(dataDir)
	cfg.PresetGlobal = map[string]string{"n-gpu-layers": "0"}
	wired, _ := wireAttached(t, dataDir, cfg, debugLogger(io.Discard))

	presets, err := os.ReadFile(llamaserver.PresetPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[*]\n", "n-gpu-layers = 0\n", "jinja = true\n", "[tiny]\n", "cache-type-k = q8_0\n", "c = 512\n",
		"model-draft = " + filepath.Join(dataDir, "models", "org", "tiny", "tiny-mtp.gguf") + "\n", "spec-type = draft-mtp\n",
	} {
		if !strings.Contains(string(presets), want) {
			t.Errorf("the preset file lacks %q:\n%s", want, presets)
		}
	}
	if strings.Contains(string(presets), "[big]") || strings.Contains(string(presets), "mmproj") {
		t.Errorf("what is not pulled stays out:\n%s", presets)
	}

	srv := httptest.NewServer(wired.Handler)
	t.Cleanup(srv.Close)
	models, err := client.New(srv.URL, "").Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, m := range models {
		status[m.Name] = m.Status
	}
	if status["tiny"] != "pulled" || status["big"] != "available" {
		t.Fatalf("the data directory's registry is the catalog: %v", status)
	}
}

func TestWireStampsTheBuildAndTheVersionOnWhatItServes(t *testing.T) {
	previous := version.Version
	version.Version = "9.9.9-test"
	t.Cleanup(func() { version.Version = previous })
	dataDir := t.TempDir()
	wired, _ := wireAttached(t, dataDir, attachedConfig(dataDir), debugLogger(io.Discard))
	lock, _ := runtime.LoadLock()
	srv := httptest.NewServer(wired.Handler)
	t.Cleanup(srv.Close)

	v, err := client.New(srv.URL, "").Version(context.Background())
	if err != nil || v.Version != "9.9.9-test" || v.Runtime.Build != lock.Build || v.Runtime.Backend != "llama.cpp" || v.Platform == "" || strings.HasPrefix(v.Platform, "/") {
		t.Fatalf("version: %+v %v", v, err)
	}
	resp, _ := get(t, srv.URL+"/v1/models")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Taraengine-Version") != "9.9.9-test" {
		t.Fatalf("the proxy stamps the version: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestWireReportsTheRuntimeItFindsThroughTheAPI(t *testing.T) {
	dataDir := t.TempDir()
	dir := installRuntime(t, dataDir)
	wired, _ := wireAttached(t, dataDir, attachedConfig(dataDir), debugLogger(io.Discard))
	srv := httptest.NewServer(wired.Handler)
	t.Cleanup(srv.Close)
	doc, err := client.New(srv.URL, "").Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Runtime.Installed || !doc.Runtime.Verified || doc.Runtime.Dir != dir || doc.Runtime.Error != "" {
		t.Fatalf("runtime: %+v", doc.Runtime)
	}
	if doc.Machine.OS == "" || doc.Machine.Arch == "" || doc.Machine.Tier == "" {
		t.Fatalf("machine: %+v", doc.Machine)
	}
}

func TestWireWarnsAboutAnOpenListenAddressWithoutAKey(t *testing.T) {
	for _, c := range []struct {
		name, listen, key string
		warned            bool
	}{
		{"open and keyless", "0.0.0.0:8272", "", true},
		{"open with a key", "0.0.0.0:8272", "daemon-key", false},
		{"local and keyless", "127.0.0.1:8272", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logs syncBuffer
			dataDir := t.TempDir()
			cfg := attachedConfig(dataDir)
			cfg.Listen, cfg.APIKey = c.listen, c.key
			wireAttached(t, dataDir, cfg, debugLogger(&logs))
			got := strings.Contains(logs.String(), "listening beyond localhost without an api_key")
			if got != c.warned {
				t.Fatalf("warned %v, want %v:\n%s", got, c.warned, logs.String())
			}
			if c.warned && !strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "daemon-key") {
				t.Fatalf("the warning names the address and never a key:\n%s", logs.String())
			}
		})
	}
}

func TestWireNeverLogsASecret(t *testing.T) {
	var logs syncBuffer
	dataDir := t.TempDir()
	cfg := attachedConfig(dataDir)
	cfg.Listen, cfg.APIKey, cfg.HFToken = "0.0.0.0:8272", "daemon-key-123", "hf-token-456"
	fake := llamatest.New(t, llamatest.Options{APIKey: "attach-key-789", PresetPath: llamaserver.PresetPath(dataDir)})
	wired, err := Wire(context.Background(), cfg, WireOptions{Attach: fake.URL(), AttachKey: "attach-key-789", Logger: debugLogger(&logs)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wired.Backend.Stop(context.Background()) })
	srv := httptest.NewServer(wired.Handler)
	t.Cleanup(srv.Close)
	c := client.New(srv.URL, "daemon-key-123")
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.New(srv.URL, "wrong-key").Models(context.Background()); err == nil {
		t.Fatal("a wrong key must be refused")
	}
	if _, err := c.Load(context.Background(), "nope", 0, false); err == nil {
		t.Fatal("an unknown model must be refused")
	}
	local := filepath.Join(t.TempDir(), "mine.gguf")
	writeText(t, local, "x")
	if err := c.Pull(context.Background(), local, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), "mine"); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"daemon-key-123", "hf-token-456", "attach-key-789", "wrong-key"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the log holds %q:\n%s", secret, logs.String())
		}
	}
	if logs.String() == "" {
		t.Fatal("the test saw no log at all, so it proves nothing")
	}
}

func TestWireRefusesWhatItCannotRun(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		prep   func(dataDir string)
		cfg    func(*config.Config)
		attach string
		want   string
	}{
		{"a data directory that is a file", nil, func(c *config.Config) { c.DataDir = file }, "http://127.0.0.1:1", "data dir"},
		{"a registry override that is broken", func(d string) { writeText(t, filepath.Join(d, "registry.yaml"), "entries: [x") }, nil,
			"http://127.0.0.1:1", "registry: parse"},
		{"a models index that is broken", func(d string) { writeText(t, filepath.Join(d, "models", "index.json"), "{not json") }, nil,
			"http://127.0.0.1:1", "models"},
		{"a router address that is no URL", nil, nil, "nonsense", "must look like http://host:port"},
		{"a router that is not there", nil, nil, "http://127.0.0.1:1", "backend is not running"},
		{"no runtime and no router", nil, nil, "", "taraengine runtime install"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dataDir := t.TempDir()
			if c.prep != nil {
				c.prep(dataDir)
			}
			cfg := attachedConfig(dataDir)
			if c.cfg != nil {
				c.cfg(&cfg)
			}
			wired, err := Wire(context.Background(), cfg, WireOptions{Attach: c.attach, Logger: debugLogger(io.Discard)})
			if err == nil {
				_ = wired.Backend.Stop(context.Background())
				t.Fatal("Wire accepted it")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func writeText(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWireCreatesTheDataDirectoryPrivately(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "nested", "taraengine")
	wireAttached(t, dataDir, attachedConfig(dataDir), debugLogger(io.Discard))
	info, err := os.Stat(dataDir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&^0o750 != 0 {
		t.Fatalf("the data directory must allow nothing beyond 0750: %v %v", info, err)
	}
}

func TestResolveRuntime(t *testing.T) {
	lock, _ := runtime.LoadLock()
	pinned := t.TempDir()
	pinnedDir := installRuntime(t, pinned)
	empty := &runtime.Installer{Dir: runtimeRoot(t.TempDir())}
	inst := &runtime.Installer{Dir: runtimeRoot(pinned)}
	own := t.TempDir()
	writeText(t, filepath.Join(own, "manifest.json"), "{not json")
	hint := "taraengine runtime install"

	dir, build, err := resolveRuntime(config.Config{}, inst, lock, false)
	if err != nil || dir != pinnedDir || build != lock.Build {
		t.Fatalf("the verified pinned build: %q %q %v", dir, build, err)
	}
	if _, _, err = resolveRuntime(config.Config{}, empty, lock, false); err == nil ||
		!strings.HasPrefix(err.Error(), "no verified runtime: ") || strings.Count(err.Error(), hint) != 1 {
		t.Fatalf("nothing installed names the install once: %v", err)
	}
	if dir, build, err = resolveRuntime(config.Config{}, empty, lock, true); err != nil || build != lock.Build || dir == "" {
		t.Fatalf("a router that is attached needs no runtime: %q %q %v", dir, build, err)
	}
	dir, build, err = resolveRuntime(config.Config{RuntimeDir: pinnedDir}, empty, lock, false)
	if err != nil || dir != pinnedDir || build != lock.Build {
		t.Fatalf("a verified runtime_dir: %q %q %v", dir, build, err)
	}
	if _, _, err = resolveRuntime(config.Config{RuntimeDir: own}, inst, lock, false); err == nil ||
		!strings.HasPrefix(err.Error(), "runtime_dir "+own+" ") || strings.Contains(err.Error(), hint) ||
		!strings.Contains(err.Error(), "check runtime_dir in the config") {
		t.Fatalf("a runtime_dir that does not verify names the directory and the key, and not the installer: %v", err)
	}
	if dir, build, err = resolveRuntime(config.Config{RuntimeDir: own}, inst, lock, true); err != nil || dir != own || build != "" {
		t.Fatalf("a bad runtime_dir does not matter for an attached router: %q %q %v", dir, build, err)
	}
}

func TestARefusedServeSaysRuntimeOnceAndNamesTheInstallOnce(t *testing.T) {
	lock, _ := runtime.LoadLock()
	tampered := t.TempDir()
	writeText(t, runtime.Binary(installRuntime(t, tampered)), "tampered")
	_, _, err := resolveRuntime(config.Config{}, &runtime.Installer{Dir: runtimeRoot(tampered)}, lock, false)
	if err == nil || !strings.HasPrefix(err.Error(), "no verified runtime: llama-server in ") ||
		strings.Contains(err.Error(), "runtime: runtime") || strings.Count(err.Error(), "taraengine runtime install") != 1 {
		t.Fatalf("a runtime that does not verify is named without its package prefix: %v", err)
	}
	broken := t.TempDir()
	writeText(t, filepath.Join(installRuntime(t, broken), "manifest.json"), "{not json")
	_, _, err = resolveRuntime(config.Config{}, &runtime.Installer{Dir: runtimeRoot(broken)}, lock, false)
	if err == nil || !strings.HasPrefix(err.Error(), "no verified runtime: manifest.json: ") ||
		strings.Count(err.Error(), "taraengine runtime install") != 1 {
		t.Fatalf("an unreadable manifest: %v", err)
	}
}

func TestInstallHintIsAddedOnlyWhereItIsMissing(t *testing.T) {
	if got := installHint("not installed, run `taraengine runtime install`"); got != "" {
		t.Errorf("already there: %q", got)
	}
	if got := installHint("open manifest.json: no such file"); got != "; run `taraengine runtime install`" {
		t.Errorf("missing: %q", got)
	}
}

func TestPlatformKey(t *testing.T) {
	for _, c := range []struct{ goos, goarch, driver, want string }{
		{"darwin", "arm64", "", "darwin/arm64"},
		{"linux", "amd64", "570.211.01", "linux/amd64/cuda12"},
		{"linux", "amd64", "580.65", "linux/amd64/cuda13"},
		{"linux", "arm64", "", "linux/arm64/cpu"},
		{"freebsd", "amd64", "", "freebsd/amd64"},
		{"darwin", "amd64", "", "darwin/amd64"},
	} {
		if got := platformKey(c.goos, c.goarch, c.driver); got != c.want {
			t.Errorf("%s/%s driver %q: got %q, want %q", c.goos, c.goarch, c.driver, got, c.want)
		}
	}
}

func TestLoadRegistry(t *testing.T) {
	embedded, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	reg, err := loadRegistry(dataDir)
	if err != nil || len(reg.Entries) != len(embedded.Entries) {
		t.Fatalf("without an override the embedded catalog is used: %v %v", reg, err)
	}
	writeText(t, filepath.Join(dataDir, "registry.yaml"), tinyRegistry)
	if reg, err = loadRegistry(dataDir); err != nil || len(reg.Entries) != 2 || reg.Entries[0].Name != "tiny" {
		t.Fatalf("the data directory's catalog replaces it: %v %v", reg, err)
	}
	writeText(t, filepath.Join(dataDir, "registry.yaml"), "entries: [x")
	if _, err = loadRegistry(dataDir); err == nil {
		t.Fatal("an override that does not parse must not fall back to the embedded catalog")
	}
	blocked := filepath.Join(t.TempDir(), "a-file")
	writeText(t, blocked, "x")
	if _, err = loadRegistry(blocked); err == nil || !strings.Contains(err.Error(), "registry override") {
		t.Fatalf("an override that cannot even be looked for is an error, not an absent one: %v", err)
	}
}

func TestWiredHandlerAnswersTheHealthRouteWithoutAKey(t *testing.T) {
	dataDir := t.TempDir()
	cfg := attachedConfig(dataDir)
	cfg.APIKey = "daemon-key"
	wired, _ := wireAttached(t, dataDir, cfg, debugLogger(io.Discard))
	srv := httptest.NewServer(wired.Handler)
	t.Cleanup(srv.Close)
	resp, body := get(t, srv.URL+"/api/health")
	var health struct{ Status string }
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &health) != nil || health.Status != "ok" {
		t.Fatalf("health: %d %s", resp.StatusCode, body)
	}
	if resp, _ = get(t, srv.URL+"/api/models"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("every other route wants the key: %d", resp.StatusCode)
	}
}

func TestWireLogsThroughTheDefaultLoggerWhenGivenNone(t *testing.T) {
	var logs syncBuffer
	previous := slog.Default()
	slog.SetDefault(debugLogger(&logs))
	t.Cleanup(func() { slog.SetDefault(previous) })
	dataDir := t.TempDir()
	cfg := attachedConfig(dataDir)
	cfg.Listen = "0.0.0.0:8272"
	fake := llamatest.New(t, llamatest.Options{APIKey: "k", PresetPath: llamaserver.PresetPath(dataDir)})
	wired, err := Wire(context.Background(), cfg, WireOptions{Attach: fake.URL(), AttachKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wired.Backend.Stop(context.Background()) })
	if !strings.Contains(logs.String(), "listening beyond localhost without an api_key") {
		t.Fatalf("the default logger was not used:\n%s", logs.String())
	}
}

func TestWireSaysWhenTheEmbeddedLockIsBroken(t *testing.T) {
	previous := pinnedLock
	pinnedLock = func() (runtime.Lock, error) { return runtime.Lock{}, errors.New("lock is broken") }
	t.Cleanup(func() { pinnedLock = previous })
	dataDir := t.TempDir()
	if _, err := Wire(context.Background(), attachedConfig(dataDir), WireOptions{Attach: "http://127.0.0.1:1"}); err == nil || err.Error() != "lock is broken" {
		t.Fatalf("%v", err)
	}
}

// TestPresetModelsLetPresetGlobalWinOverACatalogPreset: a key the operator sets in preset_global applies to every
// model, so a catalog preset for the same key, in either of its names, steps aside for it.
func TestPresetModelsLetPresetGlobalWinOverACatalogPreset(t *testing.T) {
	cat, _, _ := tinyCatalog(t)
	cfg := config.Default()
	cfg.PresetGlobal = map[string]string{"ctk": "f16"}
	models, err := presetModels(cat, cfg)(context.Background(), nil)
	if err != nil || models[0].Name != "tiny" {
		t.Fatalf("models %+v, err %v", models, err)
	}
	if _, kept := models[0].Extra["cache-type-k"]; kept {
		t.Fatalf("preset_global's ctk did not win over the catalog's cache-type-k: %+v", models[0])
	}
	cfg.PresetGlobal = map[string]string{"flash-attn": "off"}
	if models, _ = presetModels(cat, cfg)(context.Background(), nil); models[0].Extra["cache-type-k"] != "q8_0" {
		t.Fatalf("a preset_global key the catalog does not set dropped its preset: %+v", models[0])
	}
}

// TestWithoutGlobalKeysDropsAQuantizedVCacheWhenFlashAttentionIsOff: llama.cpp needs flash attention for a
// quantized V cache, so an operator who turns it off keeps every catalog model loadable, with an f16 V cache.
func TestWithoutGlobalKeysDropsAQuantizedVCacheWhenFlashAttentionIsOff(t *testing.T) {
	preset := map[string]string{"cache-type-k": "q8_0", "cache-type-v": "q8_0"}
	kOnly := map[string]string{"cache-type-k": "q8_0"}
	cases := []struct {
		name   string
		global map[string]string
		want   map[string]string
	}{
		{"flash-attn off", map[string]string{"flash-attn": "off"}, kOnly},
		{"the short name, false", map[string]string{"fa": "False"}, kOnly},
		{"auto", map[string]string{"flash-attn": "auto"}, preset},
		{"the operator's own V cache", map[string]string{"ctv": "f16"}, kOnly},
		{"nothing set", nil, preset},
	}
	for _, c := range cases {
		if got := withoutGlobalKeys(preset, c.global); !maps.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
