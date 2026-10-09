package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/pkg/api"
)

func TestLoadAndUnloadRoutes(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusPulled})
	resp, data := h.do(t, http.MethodPost, "/api/load", "", api.LoadRequest{Model: "gemma4:12b", Context: 8192})
	if resp.StatusCode != 200 || decode[api.Model](t, data).Status != "loaded" || h.fake.Loads[0].Options.Context != 8192 {
		t.Fatalf("load: %d %s %+v", resp.StatusCode, data, h.fake.Loads)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/load", "", api.LoadRequest{Model: "../x"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad name: %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/load", "", `not json`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad body: %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/load", "", api.LoadRequest{Model: "nope"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown: %d", resp.StatusCode)
	}
	cases := map[error]int{backend.ErrFaulty: http.StatusConflict, backend.ErrLoadFailed: http.StatusBadGateway, backend.ErrLoadTimeout: http.StatusGatewayTimeout, backend.ErrNotRunning: http.StatusServiceUnavailable}
	for err, want := range cases {
		h.fake.Fail("load", err)
		resp, data := h.do(t, http.MethodPost, "/api/load", "", api.LoadRequest{Model: "gemma4:12b"})
		if resp.StatusCode != want {
			t.Errorf("%v: %d, want %d", err, resp.StatusCode, want)
		}
		if err == backend.ErrFaulty && !strings.Contains(decode[api.Error](t, data).Error.Hint, "force") {
			t.Errorf("faulty hint missing: %s", data)
		}
	}
	h.fake.Fail("load", nil)
	if resp, _ := h.do(t, http.MethodPost, "/api/unload", "", api.UnloadRequest{Model: "gemma4:12b"}); resp.StatusCode != http.StatusNoContent || h.fake.Unloads[0] != "gemma4:12b" {
		t.Fatalf("unload: %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/unload", "", api.UnloadRequest{Model: "nope"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unload unknown: %d", resp.StatusCode)
	}
}

func TestLoadErrorsCarryTheirHintsAndNeverSpellACommandLineFlag(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusPulled})
	hints := map[error]string{
		backend.ErrUnknownModel: "list",
		backend.ErrFaulty:       "force",
		backend.ErrLoadFailed:   "doctor",
		backend.ErrLoadTimeout:  "load_timeout",
		backend.ErrNotRunning:   "retry",
	}
	for err, want := range hints {
		h.fake.Fail("load", err)
		_, data := h.do(t, http.MethodPost, "/api/load", "", api.LoadRequest{Model: "gemma4:12b"})
		body := decode[api.Error](t, data).Error
		if !strings.Contains(body.Hint, want) {
			t.Errorf("%v: hint %q lacks %q", err, body.Hint, want)
		}
		if !strings.Contains(body.Message, err.Error()) {
			t.Errorf("%v: message %q", err, body.Message)
		}
		if strings.Contains(body.Message+body.Hint, "--") {
			t.Errorf("%v: an error text spells a command line flag: %+v", err, body)
		}
	}
}

func TestLoadPassesTheRequestThrough(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusPulled})
	if resp, _ := h.do(t, http.MethodPost, "/api/load", "", api.LoadRequest{Model: "gemma4:12b", Context: 4096, Force: true}); resp.StatusCode != 200 {
		t.Fatalf("load: %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/load", "", api.LoadRequest{Model: "gemma4:12b"}); resp.StatusCode != 200 {
		t.Fatalf("load without options: %d", resp.StatusCode)
	}
	if len(h.fake.Loads) != 2 {
		t.Fatalf("loads: %+v", h.fake.Loads)
	}
	if got := h.fake.Loads[0]; got.Name != "gemma4:12b" || got.Options != (backend.LoadOptions{Context: 4096, Draft: true, Force: true}) {
		t.Fatalf("first load: %+v", got)
	}
	if got := h.fake.Loads[1]; got.Options != (backend.LoadOptions{Draft: true}) {
		t.Fatalf("a load with no options keeps the preset's context and does not force: %+v", got)
	}
}

func TestLoadAnswersWithTheModelAsTheEngineNowHoldsIt(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusPulled})
	_, data := h.do(t, http.MethodPost, "/api/load", "", api.LoadRequest{Model: "gemma4:12b", Context: 8192})
	m := decode[api.Model](t, data)
	if m.Name != "gemma4:12b" || m.Status != "loaded" || m.Context != 8192 || m.Tier != "16" || m.Family != "gemma4" {
		t.Fatalf("answer: %+v", m)
	}
}

func TestLoadBoundsTheContextAndAcceptsZero(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusPulled})
	for _, window := range []int{-1, 1, 511, 4194305, 10000000} {
		resp, data := h.do(t, http.MethodPost, "/api/load", "", api.LoadRequest{Model: "gemma4:12b", Context: window})
		got := decode[api.Error](t, data).Error.Message
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(got, "512") || !strings.Contains(got, "4194304") {
			t.Errorf("context %d: %d %s", window, resp.StatusCode, data)
		}
	}
	if len(h.fake.Loads) != 0 {
		t.Fatalf("a refused load must not reach the backend: %+v", h.fake.Loads)
	}
	for _, window := range []int{0, 512, 4096, 4194304} {
		if resp, data := h.do(t, http.MethodPost, "/api/load", "", api.LoadRequest{Model: "gemma4:12b", Context: window}); resp.StatusCode != 200 {
			t.Errorf("context %d: %d %s", window, resp.StatusCode, data)
		}
	}
	if len(h.fake.Loads) != 4 {
		t.Fatalf("the four loads within the bounds reach the backend: %+v", h.fake.Loads)
	}
}

func TestLoadOfAModelOnlyTheBackendKnows(t *testing.T) {
	h := newHarness(t, "")
	h.fake.SetModels(backend.ModelStatus{Name: "ghost", Status: backend.StatusPulled})
	resp, data := h.do(t, http.MethodPost, "/api/load", "", api.LoadRequest{Model: "ghost"})
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(decode[api.Error](t, data).Error.Message, "ghost") {
		t.Fatalf("ghost: %d %s", resp.StatusCode, data)
	}
}

func TestLoadFailsWhenTheBackendCannotReportTheResult(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusPulled})
	h.fake.Fail("models", backend.ErrNotRunning)
	if resp, _ := h.do(t, http.MethodPost, "/api/load", "", api.LoadRequest{Model: "gemma4:12b"}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("load: %d", resp.StatusCode)
	}
}

func TestUnloadValidatesItsRequest(t *testing.T) {
	h := newHarness(t, "")
	for _, body := range []string{"{", `"gemma4:12b"`, ``} {
		if status, _ := h.doRaw(t, http.MethodPost, "/api/unload", "application/json", body); status != http.StatusBadRequest {
			t.Errorf("body %q: %d", body, status)
		}
	}
	for _, name := range []string{"", "../x", "a/b", strings.Repeat("y", 65)} {
		if resp, _ := h.do(t, http.MethodPost, "/api/unload", "", api.UnloadRequest{Model: name}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("name %q: %d", name, resp.StatusCode)
		}
	}
	h.fake.Fail("unload", backend.ErrNotRunning)
	if resp, _ := h.do(t, http.MethodPost, "/api/unload", "", api.UnloadRequest{Model: "gemma4:12b"}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("backend down: %d", resp.StatusCode)
	}
	if len(h.fake.Unloads) != 1 {
		t.Fatalf("only the well-formed request may reach the backend: %v", h.fake.Unloads)
	}
}
