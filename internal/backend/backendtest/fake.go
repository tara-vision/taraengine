// Package backendtest is a scripted backend for api and cli tests.
package backendtest

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/tara-vision/taraengine/internal/backend"
)

// LoadCall is one recorded Load.
type LoadCall struct {
	Name    string
	Options backend.LoadOptions
}

// Fake implements backend.Backend in memory.
type Fake struct {
	mu        sync.Mutex
	models    []backend.ModelStatus
	failures  map[string]error
	Started   bool
	Loads     []LoadCall
	Unloads   []string
	Refreshes int
	HealthVal backend.Health
}

// Compile-time check that Fake implements backend.Backend.
var _ backend.Backend = (*Fake)(nil)

// New is a healthy fake with no models.
func New() *Fake {
	return &Fake{failures: map[string]error{}, HealthVal: backend.Health{OK: true, Backend: "fake", Build: "bTEST"}}
}

// SetModels replaces the model list.
func (f *Fake) SetModels(models ...backend.ModelStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models = append([]backend.ModelStatus(nil), models...)
}

// operations are the names Fail accepts, one per Backend method that can be made to fail.
var operations = []string{"start", "stop", "models", "load", "unload", "refresh", "health"}

// Fail makes the named operation (start, stop, models, load, unload, refresh, health) return err.
// It panics on any other name, so a misspelled name fails loudly instead of never firing.
func (f *Fake) Fail(op string, err error) {
	if !slices.Contains(operations, op) {
		panic(fmt.Sprintf("backendtest: unknown operation %q, want one of %s", op, strings.Join(operations, ", ")))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[op] = err
}

func (f *Fake) failure(op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures[op]
}

// Start implements backend.Backend.
func (f *Fake) Start(context.Context) error {
	if err := f.failure("start"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Started = true
	return nil
}

// Stop implements backend.Backend.
func (f *Fake) Stop(context.Context) error {
	if err := f.failure("stop"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Started = false
	return nil
}

// Models implements backend.Backend.
func (f *Fake) Models(context.Context) ([]backend.ModelStatus, error) {
	if err := f.failure("models"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]backend.ModelStatus(nil), f.models...), nil
}

// Load implements backend.Backend: the named model becomes loaded, at opts.Context when it is non-zero.
func (f *Fake) Load(_ context.Context, name string, opts backend.LoadOptions) error {
	f.mu.Lock()
	f.Loads = append(f.Loads, LoadCall{Name: name, Options: opts})
	f.mu.Unlock()
	if err := f.failure("load"); err != nil {
		return err
	}
	return f.setStatus(name, backend.StatusLoaded, opts.Context)
}

// Unload implements backend.Backend.
func (f *Fake) Unload(_ context.Context, name string) error {
	f.mu.Lock()
	f.Unloads = append(f.Unloads, name)
	f.mu.Unlock()
	if err := f.failure("unload"); err != nil {
		return err
	}
	return f.setStatus(name, backend.StatusPulled, 0)
}

// setStatus moves the named model to status. A window of 0 keeps the model's current context.
func (f *Fake) setStatus(name string, status backend.Status, window int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, m := range f.models {
		if m.Name == name {
			m.Status = status
			if window != 0 {
				m.Context = window
			}
			f.models[i] = m
			return nil
		}
	}
	return backend.ErrUnknownModel
}

// Refresh implements backend.Backend.
func (f *Fake) Refresh(context.Context) error {
	if err := f.failure("refresh"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Refreshes++
	return nil
}

// Health implements backend.Backend.
func (f *Fake) Health(context.Context) (backend.Health, error) {
	if err := f.failure("health"); err != nil {
		return backend.Health{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.HealthVal, nil
}

// Proxy implements backend.Backend with a handler that answers {"proxied":true}.
func (f *Fake) Proxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"proxied":true}`))
	})
}
