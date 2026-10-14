package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/pkg/api"
)

// detailSource is the detail a starting daemon reports, which the test moves on as the daemon would.
type detailSource struct {
	mu     sync.Mutex
	detail string
}

func (d *detailSource) set(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.detail = s
}

func (d *detailSource) get() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.detail
}

// startingHandler is the handler of a daemon that starts with cfg, reporting what source says.
func startingHandler(cfg config.Config, source *detailSource) http.Handler {
	return NewStarting(StartingDeps{
		Config: cfg, Version: "0.3.0-test", Build: "b11514", Platform: "darwin/arm64/metal",
		Detail: source.get, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// answer sends one request to h and returns the recorded answer. The request names the daemon's own address in its
// Host header, unless header gives another one.
func answer(h http.Handler, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:8272"
	for k, v := range header {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStartingHealthSaysWhatTheDaemonIsDoing(t *testing.T) {
	source := &detailSource{detail: "checking runtime"}
	h := startingHandler(config.Default(), source)
	for _, want := range []string{"checking runtime", "installing runtime b11514 (42%)"} {
		source.set(want)
		rec := answer(h, http.MethodGet, "/api/health", "", nil)
		var got api.Health
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("health: %d %v %s", rec.Code, err, rec.Body)
		}
		if got.Status != api.HealthStarting || got.Backend != "llama.cpp" || got.Detail != want {
			t.Fatalf("health %+v, want starting with %q", got, want)
		}
	}
	if rec := answer(h, http.MethodHead, "/api/health", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("HEAD health answers too: %d", rec.Code)
	}
}

func TestStartingVersionAnswersAsAlways(t *testing.T) {
	rec := answer(startingHandler(config.Default(), &detailSource{}), http.MethodGet, "/api/version", "", nil)
	var got api.Version
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("version: %d %v %s", rec.Code, err, rec.Body)
	}
	if got.Version != "0.3.0-test" || got.Runtime.Build != "b11514" || got.Platform != "darwin/arm64/metal" {
		t.Fatalf("version %+v", got)
	}
}

func TestStartingRefusesEveryOtherRouteWithRetryAfter(t *testing.T) {
	h := startingHandler(config.Default(), &detailSource{detail: "installing runtime b11514 (42%)"})
	asJSON := map[string]string{"Content-Type": "application/json"}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/models", ""},
		{http.MethodGet, "/api/doctor", ""},
		{http.MethodPost, "/api/load", `{"model":"gemma4:12b"}`},
		{http.MethodPost, "/v1/chat/completions", `{"model":"gemma4:12b"}`},
		{http.MethodGet, "/nowhere", ""},
	} {
		rec := answer(h, c.method, c.path, c.body, asJSON)
		var got api.Error
		if err := jsonDecode(rec.Body, &got); err != nil {
			t.Fatalf("%s %s: %v %s", c.method, c.path, err, rec.Body)
		}
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "5" ||
			got.Error.Message != "taraengine is starting: installing runtime b11514 (42%)" || got.Error.Hint == "" {
			t.Fatalf("%s %s: %d %q %+v", c.method, c.path, rec.Code, rec.Header().Get("Retry-After"), got)
		}
	}
}

func TestStartingKeepsTheWholeAdmissionChain(t *testing.T) {
	keyed := config.Default()
	keyed.APIKey = "secret"
	h := startingHandler(keyed, &detailSource{detail: "checking runtime"})
	if rec := answer(h, http.MethodGet, "/api/models", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a key that is set is asked for while starting too: %d", rec.Code)
	}
	if rec := answer(h, http.MethodGet, "/api/health", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("health stays open without the key: %d", rec.Code)
	}
	open := startingHandler(config.Default(), &detailSource{detail: "checking runtime"})
	if rec := answer(open, http.MethodGet, "/api/health", "", map[string]string{"Host": "evil.example:8272"}); rec.Code != http.StatusForbidden {
		t.Fatalf("a foreign Host is refused without a key: %d", rec.Code)
	}
	if rec := answer(open, http.MethodGet, "/api/health", "", map[string]string{"Origin": "https://evil.example"}); rec.Code != http.StatusForbidden {
		t.Fatalf("a foreign page is refused: %d", rec.Code)
	}
	if rec := answer(open, http.MethodPost, "/api/load", "x", map[string]string{"Content-Type": "text/plain"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("the JSON gate stands in front of the refusal: %d", rec.Code)
	}
}

func jsonDecode(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }
