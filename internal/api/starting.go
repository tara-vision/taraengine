package api

import (
	"log/slog"
	"net/http"

	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/pkg/api"
)

// startingPrefix opens the refusal of every route but health and version while the daemon starts.
const startingPrefix = "taraengine is starting: "

// hintStarting is what a refused client can do while the daemon starts.
const hintStarting = "it answers once it is ready; GET /api/health says what it is doing"

// StartingDeps are what the handler of a starting daemon needs. Detail says what the daemon is doing now; it is read
// on every request, so it may change while the handler serves.
type StartingDeps struct {
	Config   config.Config
	Version  string
	Build    string
	Platform string
	Detail   func() string
	Logger   *slog.Logger
}

// NewStarting is the handler a daemon serves while it gets ready, before the backend runs: GET /api/health answers
// that it is starting and what it is doing, GET /api/version answers as always, and every other route, /v1 included,
// is refused with 503 and a Retry-After. It stands behind the same host guard, origin rule, key and JSON rule as the
// full handler, so a starting daemon is no easier to reach than a running one.
func NewStarting(deps StartingDeps) http.Handler {
	s := newServer(Deps{
		Config: deps.Config, Version: deps.Version, Build: deps.Build, Platform: deps.Platform, Logger: deps.Logger,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, api.Health{Status: api.HealthStarting, Backend: "llama.cpp", Detail: deps.Detail()})
	})
	mux.HandleFunc("GET /api/version", s.version)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, startingPrefix+deps.Detail(), hintStarting)
	})
	return hostGuard(deps.Config, s.originPolicy(bearer(deps.Config.APIKey, jsonOnly(mux))))
}
