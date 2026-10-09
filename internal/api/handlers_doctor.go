package api

import (
	"net/http"

	"github.com/tara-vision/taraengine/internal/registry"
	"github.com/tara-vision/taraengine/pkg/api"
)

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	h, err := s.deps.Backend.Health(r.Context())
	out := api.Health{Status: "ok", Backend: h.Backend, Detail: h.Detail}
	if err != nil || !h.OK {
		out.Status = "degraded"
		if err != nil {
			s.deps.Logger.Warn("backend health probe failed", "error", err)
			out.Detail = "the backend did not answer its health probe" // this route is open, so nothing from err
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, api.Version{
		Version:  s.deps.Version,
		Runtime:  api.RuntimeInfo{Build: s.deps.Build, Backend: "llama.cpp"},
		Platform: s.deps.Platform,
	})
}

func (s *Server) doctor(w http.ResponseWriter, r *http.Request) {
	info := s.deps.Machine(r.Context())
	rec := map[string]string{}
	for _, tier := range registry.Tiers {
		if e, ok := s.deps.Catalog.Registry.Default(tier); ok {
			rec[tier] = e.Name
		}
	}
	writeJSON(w, http.StatusOK, api.Doctor{
		Machine: api.Machine{
			OS: info.OS, Arch: info.Arch, GPU: info.GPU, GPUMemoryBytes: info.GPUMemoryBytes,
			UnifiedMemory: info.UnifiedMemory, RAMBytes: info.RAMBytes, Tier: info.Tier, DriverVersion: info.DriverVersion,
		},
		Runtime:        s.deps.Runtime(),
		Recommendation: rec,
	})
}
