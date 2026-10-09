package api

import (
	"context"
	"fmt"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/catalog"
	"github.com/tara-vision/taraengine/pkg/api"
)

// otherTier is the context tier of a model with no registry entry; the serve wiring renders the presets of these
// models with the same key.
const otherTier = "other"

// view is a catalog item with what the backend says about it.
type view struct {
	item   catalog.Item
	status *backend.ModelStatus
}

// views joins every catalog item with the backend's status for it. The backend only speaks for pulled models.
func (s *Server) views(ctx context.Context) ([]view, error) {
	items, err := s.deps.Catalog.List()
	if err != nil {
		return nil, catalogError{err}
	}
	statuses, err := s.deps.Backend.Models(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]backend.ModelStatus, len(statuses))
	for _, st := range statuses {
		byName[st.Name] = st
	}
	out := make([]view, 0, len(items))
	for _, it := range items {
		v := view{item: it}
		if st, ok := byName[it.Name]; ok && it.Pulled {
			v.status = &st
		}
		out = append(out, v)
	}
	return out, nil
}

// lookup is the view of the model with that name.
func (s *Server) lookup(ctx context.Context, name string) (view, bool, error) {
	views, err := s.views(ctx)
	if err != nil {
		return view{}, false, err
	}
	for _, v := range views {
		if v.item.Name == name {
			return v, true, nil
		}
	}
	return view{}, false, nil
}

func (s *Server) model(v view) api.Model {
	m := s.catalogModel(v.item)
	if v.status == nil {
		return m
	}
	return withStatus(m, v.item, *v.status)
}

// catalogModel is what the catalog alone says about an item.
func (s *Server) catalogModel(it catalog.Item) api.Model {
	m := api.Model{
		Name: it.Name, Status: string(backend.StatusAvailable), Size: it.Size, Capabilities: []string{},
		CapabilitiesInferred: it.Inferred,
	}
	tier, native := otherTier, 0
	if e := it.Entry; e != nil {
		m.Family, m.Params, m.Quantization = e.Family, e.Params, e.Quantization
		m.Tier, m.Default, m.NativeContext = e.Tier, e.Default, e.Context
		m.Capabilities = append(m.Capabilities, e.Capabilities...)
		tier, native = e.Tier, e.Context
	}
	if it.Pulled {
		m.Status = string(backend.StatusPulled)
	}
	m.Context = s.deps.Config.ContextFor(tier, native)
	return m
}

// withStatus lays the backend's status over m and returns the result; m itself is not changed.
func withStatus(m api.Model, it catalog.Item, st backend.ModelStatus) api.Model {
	m.Status = string(st.Status)
	if st.Context > 0 {
		m.Context = st.Context
	}
	if st.Memory != nil {
		m.Memory = &api.Memory{Bytes: st.Memory.Bytes, Source: st.Memory.Source}
	}
	if st.Failed {
		m.ExitCode = st.ExitCode
		m.Error = fmt.Sprintf("the last load exited with exit code %d", st.ExitCode)
	}
	if it.Inferred && st.Props != nil {
		m.Capabilities = inferred(*st.Props)
	}
	return m
}

func inferred(p backend.Props) []string {
	caps := []string{}
	if p.SupportsTools {
		caps = append(caps, "tools")
	}
	if p.Thinking {
		caps = append(caps, "thinking")
	}
	if p.Vision {
		caps = append(caps, "vision")
	}
	return caps
}

// resident reports whether the engine keeps the model: loaded, or asleep and ready to wake on the next request.
func resident(st backend.Status) bool {
	return st == backend.StatusLoaded || st == backend.StatusSleeping
}

// running reports whether the backend has a process for the model, which a delete must stop first.
func running(st backend.Status) bool {
	return resident(st) || st == backend.StatusLoading
}
