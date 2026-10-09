// Package backend is the boundary between the daemon and whatever runs the models. The first
// implementation supervises llama-server in router mode (package llamaserver).
package backend

import (
	"context"
	"errors"
	"net/http"
)

// Status is a model's state as the API reports it.
type Status string

// The statuses a model moves through.
const (
	StatusAvailable Status = "available" // in the catalog, not pulled
	StatusPulled    Status = "pulled"    // on disk, not loaded
	StatusLoading   Status = "loading"
	StatusLoaded    Status = "loaded"
	StatusSleeping  Status = "sleeping" // unloaded after idling, reloads on the next request
	StatusFailed    Status = "failed"   // the last load failed
	StatusFaulty    Status = "faulty"   // two consecutive failed loads; refused until forced
)

// Memory is what a loaded model holds, measured on the GPU or estimated from resident memory.
type Memory struct {
	Bytes  int64  `json:"bytes"`
	Source string `json:"source"` // measured | estimated
}

// Props is what the running model reports about itself.
type Props struct {
	ContextLength int
	ModelPath     string
	SupportsTools bool
	Vision        bool
	Thinking      bool
}

// ModelStatus is one model as the backend sees it.
type ModelStatus struct {
	Name     string
	Status   Status
	Context  int
	Memory   *Memory
	ExitCode int
	Failed   bool
	LogTail  string
	Props    *Props
}

// LoadOptions tune one load.
type LoadOptions struct {
	Context int  // 0 keeps the preset's context
	Draft   bool // use the speculative-decoding sibling when the model has one
	Force   bool // load a model marked faulty
}

// Health is the backend's state.
type Health struct {
	OK      bool
	Backend string
	Build   string
	Detail  string
}

// Backend runs models.
type Backend interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Models(ctx context.Context) ([]ModelStatus, error)
	Load(ctx context.Context, name string, opts LoadOptions) error
	Unload(ctx context.Context, name string) error
	Refresh(ctx context.Context) error
	Health(ctx context.Context) (Health, error)
	Proxy() http.Handler
}

// Errors every backend reports with errors.Is.
var (
	ErrNotRunning   = errors.New("backend is not running")
	ErrUnknownModel = errors.New("unknown model")
	ErrFaulty       = errors.New("model is marked faulty after two failed loads; a forced load retries it")
	ErrLoadFailed   = errors.New("model failed to load")
	ErrLoadTimeout  = errors.New("model did not load before the timeout")
)
