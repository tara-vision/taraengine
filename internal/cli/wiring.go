package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"time"

	"github.com/tara-vision/taraengine/internal/api"
	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/backend/llamaserver"
	"github.com/tara-vision/taraengine/internal/catalog"
	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/measure"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
	"github.com/tara-vision/taraengine/internal/runtime"
	"github.com/tara-vision/taraengine/internal/state"
	"github.com/tara-vision/taraengine/internal/version"
	apitypes "github.com/tara-vision/taraengine/pkg/api"
)

// otherTier is the context tier of a model that is not in the registry; the API reports its window by the same key.
const otherTier = "other"

// WireOptions are the serve flags, and the data directory's lock when serve took it before wiring.
type WireOptions struct {
	Attach    string
	AttachKey string
	Service   bool // run by a service manager: serve --service
	Logger    *slog.Logger
	HeldLock  *os.File
}

// Wired is a daemon ready to listen.
type Wired struct {
	Handler http.Handler
	Backend *llamaserver.Backend
	Config  config.Config
}

// Wire builds the catalog, checks the runtime, starts the backend, gives the imports of an earlier version their
// native window, and returns the handler.
func Wire(ctx context.Context, cfg config.Config, opts WireOptions) (*Wired, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	if cfg.ListenBeyondLocalhost() && cfg.APIKey == "" {
		opts.Logger.Warn("listening beyond localhost without an api_key; anyone on the network can use this engine",
			"listen", cfg.Listen)
	}
	reg, override, err := loadRegistry(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	store := pull.Store{Dir: filepath.Join(cfg.DataDir, "models")}
	index := &catalog.Index{Path: filepath.Join(store.Dir, "index.json")}
	cat := catalog.Catalog{Registry: reg, Store: store, Index: index}
	if override { // the embedded catalog tells a copied entry from one the operator wrote
		if cat.Embedded, err = registry.Load(); err != nil {
			return nil, err
		}
	}
	lock, err := pinnedLock()
	if err != nil {
		return nil, err
	}
	inst := &runtime.Installer{Dir: runtimeRoot(cfg.DataDir)}
	runtimeDir, build, err := resolveRuntime(cfg, inst, lock, opts.Attach != "")
	if err != nil {
		return nil, err
	}
	be, err := newBackend(cfg, cat, runtimeDir, build, opts)
	if err != nil {
		return nil, err
	}
	if err := be.Start(ctx); err != nil {
		return nil, err
	}
	backfillNativeContexts(ctx, index, be, opts.Logger)
	platform := platformKey(goruntime.GOOS, goruntime.GOARCH, runtime.DriverVersion(ctx, runtime.ExecRunner))
	handler := api.New(api.Deps{
		Backend: be, Catalog: cat, Config: cfg, Fetcher: api.NewHFFetcher(cfg.HFToken),
		Machine: detectMachine,
		Runtime: func() apitypes.RuntimeStatus {
			st := runtimeStatus(cfg.RuntimeDir, inst, lock)
			return apitypes.RuntimeStatus{
				Installed: st.Installed, Verified: st.Verified, Build: st.Build, Dir: st.Dir, Error: st.Err,
			}
		},
		Version: version.Version, Build: build, Platform: platform, Logger: opts.Logger,
	})
	return &Wired{Handler: handler, Backend: be, Config: cfg}, nil
}

// backfillNativeContexts gives the imports that an earlier version pulled the window their files were trained for,
// and has the router re-read the presets when any got one. It runs once the backend holds the data directory, so
// that no other daemon rewrites the index meanwhile. What fails is logged, and the daemon starts with the presets
// it has.
func backfillNativeContexts(ctx context.Context, index *catalog.Index, be *llamaserver.Backend, logger *slog.Logger) {
	filled, missed, err := index.FillNativeContexts()
	for _, name := range slices.Sorted(maps.Keys(missed)) {
		logger.Warn("the native context window of an import is unknown", "model", name, "error", missed[name])
	}
	if err != nil {
		logger.Warn("could not save the native context windows of the imports", "error", err)
		return
	}
	if filled == 0 {
		return
	}
	if err := be.Refresh(ctx); err != nil {
		logger.Warn("the router did not pick up the native context windows of the imports", "error", err)
	}
}

// newBackend is the router backend for the catalog: it supervises llama-server from runtimeDir, or attaches to the
// router named in opts. Its preset file is rendered from the models the catalog holds on disk.
func newBackend(
	cfg config.Config, cat catalog.Catalog, runtimeDir, build string, opts WireOptions,
) (*llamaserver.Backend, error) {
	global := llamaserver.DefaultGlobal()
	maps.Copy(global, cfg.PresetGlobal)
	return llamaserver.New(llamaserver.Options{
		RuntimeDir: runtimeDir, DataDir: cfg.DataDir, Build: build, Version: version.Version,
		APIKey: opts.AttachKey, ModelsMax: cfg.ModelsMax, SleepIdleSeconds: cfg.SleepIdleSeconds,
		LoadTimeout: time.Duration(cfg.LoadTimeout) * time.Second, Global: global,
		Models: presetModels(cat, cfg), State: state.Store{Path: filepath.Join(cfg.DataDir, "state.json")},
		Probe: measure.Default(goruntime.GOOS, measure.ExecRunner), OnProps: saveCapabilities(cat.Index, opts.Logger),
		Logger: opts.Logger, Attach: opts.Attach, HeldLock: opts.HeldLock,
	})
}

// saveCapabilities keeps what a running import showed it can do in the index, so the catalog still knows it after
// the model stops. A registry model is not in the index, and the index leaves it alone.
func saveCapabilities(index *catalog.Index, logger *slog.Logger) func(string, backend.Props) {
	return func(name string, props backend.Props) {
		if err := index.SetCapabilities(name, props.Capabilities()); err != nil {
			logger.Warn("could not save the capabilities of an import", "model", name, "error", err)
		}
	}
}

// platformKey names the build this machine runs: the lock's key, or the bare OS and architecture where this
// release pins no build, so that the version a daemon reports never shows an empty platform.
func platformKey(goos, goarch, driver string) string {
	if p, err := runtime.Select(goos, goarch, driver); err == nil {
		return p.Key()
	}
	return goos + "/" + goarch
}

// loadRegistry is the catalog of the data directory when it carries one, else the embedded catalog; override says
// which. An override that cannot be read is an error and never a reason to fall back: the operator would be served a
// catalog other than the one they wrote.
func loadRegistry(dataDir string) (reg *registry.Registry, override bool, err error) {
	path := filepath.Join(dataDir, "registry.yaml")
	switch _, err := os.Stat(path); {
	case err == nil:
		reg, err := registry.LoadFile(path)
		return reg, true, err
	case errors.Is(err, fs.ErrNotExist):
		reg, err := registry.Load()
		return reg, false, err
	default:
		return nil, false, fmt.Errorf("registry override: %w", err)
	}
}

// modelLister is the backend's source of preset sections; the map holds the window the state remembers per model.
type modelLister = func(ctx context.Context, overrides map[string]int) ([]llamaserver.PresetModel, error)

// presetModels renders the pulled models of the catalog with their context and registry presets. The draft
// sibling of a model is part of its preset once it is pulled, which is what turns speculative decoding on.
// withoutGlobalKeys is a catalog preset without the keys the operator's preset_global sets, in either of their
// names, since the operator's choice applies to every model, the catalog's own included; and without a quantized V
// cache when preset_global turns flash attention off, which llama.cpp needs for one.
func withoutGlobalKeys(preset, global map[string]string) map[string]string {
	out := maps.Clone(preset)
	set := map[string]bool{}
	for key, value := range global {
		set[registry.CanonicalKey(key)] = true
		if registry.CanonicalKey(key) == "flash-attn" && flashOff[strings.ToLower(value)] {
			set["cache-type-v"] = true
		}
	}
	for key := range preset {
		if set[registry.CanonicalKey(key)] {
			delete(out, key)
		}
	}
	return out
}

// flashOff are the values that turn llama-server's flash attention off.
var flashOff = map[string]bool{"off": true, "false": true, "0": true, "no": true, "disabled": true}

func presetModels(cat catalog.Catalog, cfg config.Config) modelLister {
	return func(_ context.Context, overrides map[string]int) ([]llamaserver.PresetModel, error) {
		items, err := cat.List()
		if err != nil {
			return nil, err
		}
		var out []llamaserver.PresetModel
		for _, it := range items {
			if !it.Pulled {
				continue
			}
			tier := otherTier
			var extra map[string]string
			if it.Entry != nil {
				tier = it.Entry.Tier
				extra = withoutGlobalKeys(it.Entry.Preset, cfg.PresetGlobal)
			}
			window := cfg.ContextFor(tier, it.NativeContext)
			if override, ok := overrides[it.Name]; ok {
				window = override
			}
			out = append(out, llamaserver.PresetModel{
				Name: it.Name, ModelPath: it.ModelPath, MMProjPath: it.MMProjPath, DraftPath: it.DraftPath,
				DraftKind: it.DraftKind, Context: window, Extra: extra,
			})
		}
		return out, nil
	}
}
