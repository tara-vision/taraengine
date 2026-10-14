package cli

import (
	"net/http"
	"sync/atomic"
)

// The steps a starting daemon reports in the detail of its health.
const (
	stepChecking = "checking runtime"
	stepStarting = "starting llama-server"
)

// front is the handler the HTTP server serves: the starting handler from the moment the daemon binds, then the full
// handler once the daemon is wired. The switch is atomic, so a request sees one or the other.
type front struct{ h atomic.Pointer[http.Handler] }

func newFront(h http.Handler) *front {
	f := &front{}
	f.set(h)
	return f
}

func (f *front) set(h http.Handler) { f.h.Store(&h) }

func (f *front) ServeHTTP(w http.ResponseWriter, r *http.Request) { (*f.h.Load()).ServeHTTP(w, r) }

// progress is the step a starting daemon is at, which its health route reports while it starts.
type progress struct{ step atomic.Pointer[string] }

func (p *progress) set(step string) { p.step.Store(&step) }

func (p *progress) detail() string {
	if s := p.step.Load(); s != nil {
		return *s
	}
	return ""
}
