package api

import (
	"maps"
	"net/http"
	"strconv"

	"github.com/tara-vision/taraengine/internal/registry"
	"github.com/tara-vision/taraengine/pkg/api"
)

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	views, err := s.views(r.Context())
	if err != nil {
		s.backendError(w, r, err)
		return
	}
	out := make([]api.Model, 0, len(views))
	for _, v := range views {
		out = append(out, s.model(v))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listLoaded(w http.ResponseWriter, r *http.Request) {
	views, err := s.views(r.Context())
	if err != nil {
		s.backendError(w, r, err)
		return
	}
	out := []api.Loaded{}
	for _, v := range views {
		if v.status == nil || !resident(v.status.Status) {
			continue
		}
		m := s.model(v)
		out = append(out, api.Loaded{Name: m.Name, Context: m.Context, Memory: m.Memory})
	}
	writeJSON(w, http.StatusOK, out)
}

// modelName is the validated name from the path; it writes the 400 itself.
func modelName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if err := registry.ValidName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return "", false
	}
	return name, true
}

// find looks the named model up; it writes the error itself.
func (s *Server) find(w http.ResponseWriter, r *http.Request, name string) (view, bool) {
	v, found, err := s.lookup(r.Context(), name)
	switch {
	case err != nil:
		s.backendError(w, r, err)
	case !found:
		writeError(w, http.StatusNotFound, "unknown model "+name, "taraengine list shows the catalog")
	default:
		return v, true
	}
	return view{}, false
}

func (s *Server) showModel(w http.ResponseWriter, r *http.Request) {
	name, ok := modelName(w, r)
	if !ok {
		return
	}
	v, ok := s.find(w, r, name)
	if !ok {
		return
	}
	m := s.model(v)
	d := api.ModelDetail{Model: m, Preset: map[string]string{}}
	if e := v.item.Entry; e != nil {
		d.Digest = e.SHA256[:12]
		maps.Copy(d.Preset, e.Preset)
	}
	d.Preset["c"] = strconv.Itoa(m.Context)
	if v.status != nil {
		d.LogTail = v.status.LogTail
	}
	writeJSON(w, http.StatusOK, d)
}

// deleteModel removes a pulled model. It takes the name first and reads the model after, so that what it reads
// cannot change under it: a pull or another delete of the name is refused with 409. It stops the model when the
// backend holds a process for it, deletes through the store, and tells the backend last. A model that came in by
// the escape hatch and whose file has gone since, a local original that was moved, still has its index entry and
// perhaps a link that points nowhere, and the delete removes those too.
func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	name, ok := modelName(w, r)
	if !ok {
		return
	}
	release, refused := s.claim(name, opDelete)
	if refused != nil {
		refused.write(w)
		return
	}
	defer release()
	v, ok := s.find(w, r, name)
	if !ok {
		return
	}
	if !v.item.Pulled && v.item.Entry != nil {
		writeError(w, http.StatusNotFound, name+" is not pulled", "")
		return
	}
	if v.status != nil && running(v.status.Status) {
		if err := s.deps.Backend.Unload(r.Context(), name); err != nil {
			s.backendError(w, r, err)
			return
		}
	}
	if err := s.remove(v.item); err != nil {
		s.deps.Logger.Error("delete failed", "model", name, "error", err)
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	s.deps.Logger.Info("deleted", "model", name)
	if err := s.deps.Backend.Refresh(r.Context()); err != nil {
		s.backendError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
