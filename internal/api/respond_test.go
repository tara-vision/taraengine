package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/pkg/api"
)

func TestBackendAndCatalogFailuresAnswerDifferently(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	paths := []string{"/api/models", "/api/loaded", "/api/models/gemma4:12b"}

	h.fake.Fail("models", backend.ErrNotRunning)
	for _, path := range paths {
		resp, data := h.do(t, http.MethodGet, path, "", nil)
		body := decode[api.Error](t, data).Error
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body.Message, "not running") || !strings.Contains(body.Hint, "retry") {
			t.Errorf("%s with the backend down: %d %s", path, resp.StatusCode, data)
		}
	}
	h.fake.Fail("models", errors.New(transportErr))
	for _, path := range paths {
		resp, data := h.do(t, http.MethodGet, path, "", nil)
		body := decode[api.Error](t, data).Error
		if resp.StatusCode != http.StatusServiceUnavailable || body.Message != "the engine backend is unavailable" || !strings.Contains(body.Hint, "retry") {
			t.Errorf("%s with an unclassified backend error: %d %s", path, resp.StatusCode, data)
		}
		assertNoTransportText(t, path, data)
	}
	h.fake.Fail("models", nil)
	breakIndex(t, h)
	for _, path := range paths {
		resp, data := h.do(t, http.MethodGet, path, "", nil)
		body := decode[api.Error](t, data).Error
		if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(body.Message, "parse index") {
			t.Errorf("%s with a broken index: %d %s", path, resp.StatusCode, data)
		}
		if logs := h.logs.String(); !strings.Contains(logs, "catalog unreadable") {
			t.Errorf("%s: a broken index is an error for the operator, so it is logged: %s", path, logs)
		}
	}
}

// Every route that can meet a backend failure must answer a failure it cannot classify with the same fixed text,
// and keep the real error for the log.
func TestUnclassifiedBackendErrorsNeverReachTheClient(t *testing.T) {
	routes := []struct {
		name   string
		op     string // the fake operation that fails
		status backend.Status
		method string
		path   string
		body   any
	}{
		{"list", "models", backend.StatusPulled, http.MethodGet, "/api/models", nil},
		{"loaded", "models", backend.StatusPulled, http.MethodGet, "/api/loaded", nil},
		{"show", "models", backend.StatusPulled, http.MethodGet, "/api/models/gemma4:12b", nil},
		{"load", "load", backend.StatusPulled, http.MethodPost, "/api/load", api.LoadRequest{Model: "gemma4:12b"}},
		{"load then list", "models", backend.StatusPulled, http.MethodPost, "/api/load", api.LoadRequest{Model: "gemma4:12b"}},
		{"unload", "unload", backend.StatusLoaded, http.MethodPost, "/api/unload", api.UnloadRequest{Model: "gemma4:12b"}},
		{"delete unloads", "unload", backend.StatusLoaded, http.MethodDelete, "/api/models/gemma4:12b", nil},
		{"delete refreshes", "refresh", backend.StatusPulled, http.MethodDelete, "/api/models/gemma4:12b", nil},
	}
	for _, rt := range routes {
		h := newHarness(t, "")
		pulled(t, h, "gemma4:12b")
		h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: rt.status})
		h.fake.Fail(rt.op, errors.New(transportErr))
		resp, data := h.do(t, rt.method, rt.path, "", rt.body)
		body := decode[api.Error](t, data).Error
		if resp.StatusCode != http.StatusServiceUnavailable || body.Message != "the engine backend is unavailable" || body.Hint != hintRetry {
			t.Errorf("%s: %d %s", rt.name, resp.StatusCode, data)
		}
		assertNoTransportText(t, rt.name, data)
		if logs := h.logs.String(); !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "dial tcp 127.0.0.1:41234") {
			t.Errorf("%s: the real error belongs in the log at error level: %s", rt.name, logs)
		}
	}
}

func TestRequestBodiesMustBeJSON(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusPulled})
	body := `{"model":"gemma4:12b"}`
	refused := []string{
		"", "text/plain", "text/plain;charset=UTF-8", "application/x-www-form-urlencoded", "multipart/form-data",
		"application/jsonx", "json",
	}
	for _, contentType := range refused {
		for _, path := range []string{"/api/load", "/api/unload", "/api/pull"} {
			status, data := h.doRaw(t, http.MethodPost, path, contentType, body)
			got := decode[api.Error](t, data).Error
			if status != http.StatusBadRequest || got.Message != "the request body must be application/json" || !strings.Contains(got.Hint, "Content-Type: application/json") {
				t.Errorf("POST %s as %q: %d %s", path, contentType, status, data)
			}
		}
	}
	if len(h.fake.Loads) != 0 || len(h.fake.Unloads) != 0 || h.fake.Refreshes != 0 {
		t.Fatalf("a refused body must do nothing: %+v %v %d", h.fake.Loads, h.fake.Unloads, h.fake.Refreshes)
	}
	for _, contentType := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
		if status, data := h.doRaw(t, http.MethodPost, "/api/load", contentType, body); status != 200 {
			t.Errorf("load as %q: %d %s", contentType, status, data)
		}
	}
}

func TestAMalformedBodyIsNotToldToSendAContentType(t *testing.T) {
	h := newHarness(t, "")
	for _, body := range []string{"{", `{"model":`, `"gemma4:12b"`, `[]`, `{"model":5}`, ``} {
		for _, path := range []string{"/api/load", "/api/unload", "/api/pull"} {
			status, data := h.doRaw(t, http.MethodPost, path, "application/json", body)
			got := decode[api.Error](t, data).Error
			if status != http.StatusBadRequest || got.Message != "the request body must be a JSON object" || !strings.Contains(got.Hint, `{"model": "gemma4:12b"}`) {
				t.Errorf("POST %s with %q: %d %s", path, body, status, data)
			}
			if strings.Contains(got.Message+got.Hint, "Content-Type") {
				t.Errorf("POST %s with %q: a body that is sent as JSON is not told to send a content type: %s", path, body, data)
			}
		}
	}
}

func TestRequestBodiesAreCapped(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusPulled})
	// Both bodies are valid JSON for a model that exists; only their size differs.
	body := func(padding int) string {
		return `{"model":"gemma4:12b","padding":"` + strings.Repeat("a", padding) + `"}`
	}
	status, data := h.doRaw(t, http.MethodPost, "/api/load", "application/json", body((1<<20)+4096))
	if got := decode[api.Error](t, data).Error; status != http.StatusBadRequest || got.Message != "the request body is larger than 1048576 bytes" || got.Hint != "" {
		t.Fatalf("a body over the cap: %d %s", status, data)
	}
	if len(h.fake.Loads) != 0 {
		t.Fatalf("a body over the cap must not be acted on: %+v", h.fake.Loads)
	}
	if status, data := h.doRaw(t, http.MethodPost, "/api/load", "application/json", body(512<<10)); status != 200 {
		t.Fatalf("a 512 KiB body is within the cap: %d %s", status, data)
	}
}

// spyWriter records whether anything was written to it.
type spyWriter struct {
	header http.Header
	wrote  bool
}

func (s *spyWriter) Header() http.Header { return s.header }

func (s *spyWriter) Write(p []byte) (int, error) {
	s.wrote = true
	return len(p), nil
}

func (s *spyWriter) WriteHeader(int) { s.wrote = true }

func TestARequestThatEndedBeforeTheBackendAnsweredGetsNothingAndLeavesNoErrorLine(t *testing.T) {
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	late, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()
	for name, tc := range map[string]struct {
		ctx context.Context
		err error
	}{
		"the client went away":             {gone, context.Canceled},
		"the client went away, wrapped":    {gone, fmt.Errorf("router: %w", context.Canceled)},
		"the client's deadline ran out":    {late, context.DeadlineExceeded},
		"the backend reports the deadline": {late, fmt.Errorf("router: %w", context.DeadlineExceeded)},
	} {
		h := newHarness(t, "")
		h.fake.Fail("models", tc.err)
		req := httptest.NewRequest(http.MethodGet, "/api/models", nil).WithContext(tc.ctx)
		req.Host = "127.0.0.1:8272"
		w := &spyWriter{header: http.Header{}}
		New(h.deps).ServeHTTP(w, req)
		logs := h.logs.String()
		delete(w.header, "Vary") // every answer is prepared with it, and none is written here
		if w.wrote || len(w.header) != 0 {
			t.Errorf("%s: a client that is gone is written nothing: %v", name, w.header)
		}
		if !strings.Contains(logs, "level=DEBUG") || strings.Contains(logs, "level=ERROR") || strings.Contains(logs, "level=WARN") {
			t.Errorf("%s: the end of a request is a debug line, not an error: %s", name, logs)
		}
	}
}

func TestAContextErrorWhileTheRequestLivesIsAnUnavailableBackend(t *testing.T) {
	// The router's client has deadlines of its own, and a backend that is shut down cancels its work. Neither is
	// the caller going away, so the caller is still answered, and the operator still reads the error.
	for _, err := range []error{context.DeadlineExceeded, context.Canceled, fmt.Errorf("router: %w", context.DeadlineExceeded)} {
		h := newHarness(t, "")
		h.fake.Fail("models", err)
		resp, data := h.do(t, http.MethodGet, "/api/models", "", nil)
		if body := decode[api.Error](t, data).Error; resp.StatusCode != http.StatusServiceUnavailable ||
			body.Message != "the engine backend is unavailable" || body.Hint != hintRetry {
			t.Errorf("%v: %d %s", err, resp.StatusCode, data)
		}
		if logs := h.logs.String(); !strings.Contains(logs, "level=ERROR") {
			t.Errorf("%v: the operator reads it in the log: %s", err, logs)
		}
	}
}
