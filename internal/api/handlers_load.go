package api

import (
	"fmt"
	"net/http"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/registry"
	"github.com/tara-vision/taraengine/pkg/api"
)

func (s *Server) load(w http.ResponseWriter, r *http.Request) {
	var req api.LoadRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := registry.ValidName(req.Model); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	if req.Context != 0 && (req.Context < config.MinContext || req.Context > config.MaxContext) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("context %d is out of bounds: use 0 for the preset's window, "+
			"or a number of tokens from %d to %d", req.Context, config.MinContext, config.MaxContext), "")
		return
	}
	opts := backend.LoadOptions{Context: req.Context, Draft: true, Force: req.Force}
	if err := s.deps.Backend.Load(r.Context(), req.Model, opts); err != nil {
		s.backendError(w, r, err)
		return
	}
	v, found, err := s.lookup(r.Context(), req.Model)
	switch {
	case err != nil:
		s.backendError(w, r, err)
	case !found:
		writeError(w, http.StatusNotFound, "unknown model "+req.Model, "")
	default:
		writeJSON(w, http.StatusOK, s.model(v))
	}
}

func (s *Server) unload(w http.ResponseWriter, r *http.Request) {
	var req api.UnloadRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := registry.ValidName(req.Model); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	if err := s.deps.Backend.Unload(r.Context(), req.Model); err != nil {
		s.backendError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
