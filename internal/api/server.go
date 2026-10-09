package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/catalog"
	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/machine"
	"github.com/tara-vision/taraengine/pkg/api"
)

// Deps is everything the handlers need. Backend, Catalog, Config and Fetcher are required. Machine, Runtime and
// Logger may be left nil: doctor then reports empty figures, and the default logger is used.
type Deps struct {
	Backend  backend.Backend
	Catalog  catalog.Catalog
	Config   config.Config
	Fetcher  Fetcher
	Machine  func(ctx context.Context) machine.Info
	Runtime  func() api.RuntimeStatus
	Version  string
	Build    string
	Platform string
	Logger   *slog.Logger
}

// Server holds the deps and the names with a pull or a delete in flight.
type Server struct {
	deps Deps
	mu   sync.Mutex
	busy map[string]string // the operation that holds each name: a pull or a delete
}

// New builds the handler: our routes under /api, the backend proxy under /v1, the JSON rule for every POST around
// both, the bearer check around those, the origin rule around that, and the host check around the lot.
func New(deps Deps) http.Handler {
	return newServer(deps).handler()
}

func newServer(deps Deps) *Server {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Machine == nil {
		deps.Machine = func(context.Context) machine.Info { return machine.Info{} }
	}
	if deps.Runtime == nil {
		deps.Runtime = func() api.RuntimeStatus { return api.RuntimeStatus{} }
	}
	return &Server{deps: deps, busy: map[string]string{}}
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/version", s.version)
	mux.HandleFunc("GET /api/doctor", s.doctor)
	mux.HandleFunc("GET /api/models", s.listModels)
	mux.HandleFunc("GET /api/models/{name}", s.showModel)
	mux.HandleFunc("DELETE /api/models/{name}", s.deleteModel)
	mux.HandleFunc("GET /api/loaded", s.listLoaded)
	mux.HandleFunc("POST /api/load", s.load)
	mux.HandleFunc("POST /api/unload", s.unload)
	mux.HandleFunc("POST /api/pull", s.pull)
	mux.Handle("/v1/", s.deps.Backend.Proxy())
	return hostGuard(s.deps.Config, s.originPolicy(bearer(s.deps.Config.APIKey, jsonOnly(mux))))
}
