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
	"time"

	"github.com/tara-vision/taraengine/internal/api"
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

// WireOptions are the serve flags.
type WireOptions struct {
	Attach    string
	AttachKey string
	Logger    *slog.Logger
}

// Wired is a daemon ready to listen.
type Wired struct {
	Handler http.Handler
	Backend *llamaserver.Backend
	Config  config.Config
}

// Wire builds the catalog, checks the runtime, starts the backend and returns the handler.
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
	reg, err := loadRegistry(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	store := pull.Store{Dir: filepath.Join(cfg.DataDir, "models")}
	index := &catalog.Index{Path: filepath.Join(store.Dir, "index.json")}
	cat := catalog.Catalog{Registry: reg, Store: store, Index: index}
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
		Probe: measure.Default(goruntime.GOOS, measure.ExecRunner), Logger: opts.Logger, Attach: opts.Attach,
	})
}

// platformKey names the build this machine runs: the lock's key, or the bare OS and architecture where this
// release pins no build, so that the version a daemon reports never shows an empty platform.
func platformKey(goos, goarch, driver string) string {
	if p, err := runtime.Select(goos, goarch, driver); err == nil {
		return p.Key()
	}
	return goos + "/" + goarch
}

// loadRegistry is the catalog of the data directory when it carries one, else the embedded catalog. An override
// that cannot be read is an error and never a reason to fall back: the operator would be served a catalog other
// than the one they wrote.
func loadRegistry(dataDir string) (*registry.Registry, error) {
	override := filepath.Join(dataDir, "registry.yaml")
	switch _, err := os.Stat(override); {
	case err == nil:
		return registry.LoadFile(override)
	case errors.Is(err, fs.ErrNotExist):
		return registry.Load()
	default:
		return nil, fmt.Errorf("registry override: %w", err)
	}
}

// modelLister is the backend's source of preset sections; the map holds the window the state remembers per model.
type modelLister = func(ctx context.Context, overrides map[string]int) ([]llamaserver.PresetModel, error)

// presetModels renders the pulled models of the catalog with their context and registry presets. The draft
// sibling of a model is part of its preset once it is pulled, which is what turns speculative decoding on.
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
			tier, native := otherTier, 0
			var extra map[string]string
			if it.Entry != nil {
				tier, native = it.Entry.Tier, it.Entry.Context
				extra = maps.Clone(it.Entry.Preset)
			}
			window := cfg.ContextFor(tier, native)
			if override, ok := overrides[it.Name]; ok {
				window = override
			}
			out = append(out, llamaserver.PresetModel{
				Name: it.Name, ModelPath: it.ModelPath, MMProjPath: it.MMProjPath, DraftPath: it.DraftPath,
				Context: window, Extra: extra,
			})
		}
		return out, nil
	}
}
