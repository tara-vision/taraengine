package llamaserver

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/llamatest"
)

// propsSeen records what the backend hands to OnProps, and whether the backend's lock was free each time.
type propsSeen struct {
	mu        sync.Mutex
	b         *Backend
	names     []string
	props     []backend.Props
	lockTaken bool
}

func (s *propsSeen) hear(name string, props backend.Props) {
	if s.b.mu.TryLock() {
		s.b.mu.Unlock()
	} else {
		s.lockTaken = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = append(s.names, name)
	s.props = append(s.props, props)
}

func withPropsSeen(seen *propsSeen) func(*llamatest.Server, *Options) {
	return func(_ *llamatest.Server, o *Options) { o.OnProps = seen.hear }
}

func TestEveryPropsReadIsHandedOnOutsideTheLock(t *testing.T) {
	seen := &propsSeen{}
	h := newHarnessWith(t, 0, withPropsSeen(seen))
	seen.b = h.b
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	h.fake.SetStatus("b", "loaded") // a request brought b up; the next read records it
	statusOf(t, h.b, "b")
	if len(seen.names) != 2 || seen.names[0] != "a" || seen.names[1] != "b" || !seen.props[0].SupportsTools {
		t.Fatalf("the props of the load and of the read are handed on: %v %+v", seen.names, seen.props)
	}
	if seen.lockTaken {
		t.Fatal("the callback ran while the backend held its lock")
	}
}

func TestPropsThatCannotBeReadAreNotHandedOn(t *testing.T) {
	seen := &propsSeen{}
	h := behindFlaky(t, func(r *http.Request, _ int) *cannedReply {
		if r.URL.Path == "/props" {
			return scriptedFailure(http.StatusInternalServerError)
		}
		return nil
	})
	h.b.opts.OnProps = seen.hear
	seen.b = h.b
	if err := h.b.Load(context.Background(), "a", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(seen.names) != 0 {
		t.Fatalf("nothing is handed on without props: %v", seen.names)
	}
}

func TestPropsAreHandedOnUnderTheCatalogName(t *testing.T) {
	seen := &propsSeen{}
	h := newHarnessWith(t, 0, withPropsSeen(seen))
	seen.b = h.b
	withModel("my:model")(h)
	if err := h.b.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.b.Load(context.Background(), "my:model", backend.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := countRequests(h.fake, "GET /props?model=my-model&autoload=false"); n != 1 || len(seen.names) != 1 ||
		seen.names[0] != "my:model" {
		t.Fatalf("the router is asked under its id and the props are handed on under the catalog name: %d %v", n, seen.names)
	}
}
