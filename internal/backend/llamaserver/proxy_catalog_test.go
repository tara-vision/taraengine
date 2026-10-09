package llamaserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/llamatest"
)

// proxyError is the error body the proxy answers with.
type proxyError struct {
	Error struct {
		Message string
		Type    string
		Code    int
		Hint    string
	}
}

func decodeProxyError(t *testing.T, data []byte) proxyError {
	t.Helper()
	var body proxyError
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("the error body is not JSON: %v %s", err, data)
	}
	return body
}

func catalogModels(names ...string) []PresetModel {
	out := make([]PresetModel, 0, len(names))
	for _, n := range names {
		out = append(out, PresetModel{Name: n, ModelPath: "/m/" + n + ".gguf"})
	}
	return out
}

func TestProxyAnswersAnUnknownModelWithTheNearestNames(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	_, front := proxyOver(t, up, catalogModels("gemma4:12b", "gemma4:e4b", "gemma4:26b", "qwen3.6:27b", "plain")...)
	rows := []struct {
		model string
		want  string
	}{
		{"gemma4:12", `model "gemma4:12" is not a pulled model; the nearest are gemma4:12b, gemma4:26b, gemma4:e4b`},
		{"GEMMA4:12B", `the nearest are gemma4:12b, `},
		{"plan", `the nearest are plain, `},
		{strings.Repeat("x", 300), `model "` + strings.Repeat("x", 80) + `..." is not a pulled model`},
	}
	for _, row := range rows {
		resp := postJSON(t, front.URL+"/v1/chat/completions", `{"model":"`+row.model+`","messages":[]}`, nil)
		data, _ := io.ReadAll(resp.Body)
		body := decodeProxyError(t, data)
		if resp.StatusCode != http.StatusNotFound || body.Error.Code != http.StatusNotFound ||
			body.Error.Type != "invalid_request_error" || !strings.Contains(body.Error.Message, row.want) {
			t.Errorf("%s: %d %s", row.model, resp.StatusCode, data)
		}
		if resp.Header.Get("X-Taraengine-Version") != "0.1.0-test" || resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers %v", resp.Header)
		}
	}
	if n := len(up.requests()); n != 0 {
		t.Fatalf("a name outside the catalog must not reach the router, upstream saw %d", n)
	}
}

func TestProxyAnswersAnUnknownModelWhenNothingIsPulled(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	_, front := proxyOver(t, up)
	resp := postJSON(t, front.URL+"/v1/chat/completions", `{"model":"gemma4:12b"}`, nil)
	data, _ := io.ReadAll(resp.Body)
	if body := decodeProxyError(t, data); resp.StatusCode != http.StatusNotFound ||
		!strings.Contains(body.Error.Message, "no model is pulled yet") || body.Error.Hint == "" {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
}

func TestProxyRefusesAModelMarkedFaulty(t *testing.T) {
	h := newHarnessWith(t, 0, func(_ *llamatest.Server, o *Options) {
		writeTestFile(t, o.State.Path, `{"failed_loads":{"b":2,"a":1}}`)
	})
	front := httptest.NewServer(h.b.Proxy())
	t.Cleanup(front.Close)
	resp := postJSON(t, front.URL+"/v1/chat/completions", `{"model":"b","messages":[]}`, nil)
	data, _ := io.ReadAll(resp.Body)
	body := decodeProxyError(t, data)
	if resp.StatusCode != http.StatusConflict || body.Error.Code != http.StatusConflict ||
		!strings.Contains(body.Error.Message, "faulty") || !strings.Contains(body.Error.Message, ": b") {
		t.Fatalf("%d %s", resp.StatusCode, data)
	}
	if !strings.Contains(body.Error.Hint, "force") || strings.Contains(body.Error.Hint, "--") {
		t.Fatalf("the hint names the forced load, never a flag: %q", body.Error.Hint)
	}
	if n := countRequests(h.fake, "POST /v1/"); n != 0 {
		t.Fatalf("a faulty model must not be autoloaded through /v1, the router saw %d", n)
	}
	resp = postJSON(t, front.URL+"/v1/chat/completions", `{"model":"a","messages":[]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("one failed load is not faulty: %d", resp.StatusCode)
	}
}

func TestProxyRefusesAFaultyModelNamedByItsRouterID(t *testing.T) {
	h := newHarnessWith(t, 0, func(_ *llamatest.Server, o *Options) {
		writeTestFile(t, o.State.Path, `{"failed_loads":{"gemma4:12b":2}}`)
		o.Models = func(context.Context, map[string]int) ([]PresetModel, error) {
			return catalogModels("gemma4:12b"), nil
		}
	})
	front := httptest.NewServer(h.b.Proxy())
	t.Cleanup(front.Close)
	resp := postJSON(t, front.URL+"/v1/chat/completions", `{"model":"gemma4-12b"}`, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("the router id of a faulty model is refused too: %d", resp.StatusCode)
	}
}

func TestProxyRefusesABodyThatNamesNoModel(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	_, front := proxyOver(t, up, catalogModels("plain")...)
	bodies := map[string]string{
		"an empty body":            "",
		"a body without a model":   `{"messages":[]}`,
		"a model that is no text":  `{"model":7}`,
		"an empty model":           `{"model":""}`,
		"a null model":             `{"model":null}`,
		"a form":                   `model=plain`,
		"a JSON array":             `["plain"]`,
		"a JSON null":              `null`,
		"a body cut off in a name": `{"model":"pla`,
	}
	for name, body := range bodies {
		resp := postJSON(t, front.URL+"/v1/chat/completions", body, nil)
		data, _ := io.ReadAll(resp.Body)
		parsed := decodeProxyError(t, data)
		if resp.StatusCode != http.StatusBadRequest || parsed.Error.Code != http.StatusBadRequest ||
			parsed.Error.Type != "invalid_request_error" || !strings.Contains(parsed.Error.Message, "model") {
			t.Errorf("%s: %d %s", name, resp.StatusCode, data)
		}
	}
	if n := len(up.requests()); n != 0 {
		t.Fatalf("a body that names no model must not reach the router, upstream saw %d", n)
	}
}

func TestProxyListsOnlyThePulledModels(t *testing.T) {
	list := `{"data":[{"id":"gemma4-12b","object":"model"},{"id":"plain"},{"id":"stranger"},` +
		`{"object":"model"},{"id":5},{"id":"ggml-org/some-repo-GGUF:Q4_K_M"}],"object":"list"}`
	up := listUpstream(t, list)
	_, front := proxyOver(t, up, catalogModels("gemma4:12b", "plain")...)
	resp, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	var got struct {
		Data   []map[string]any
		Object string
	}
	if err := json.Unmarshal(data, &got); err != nil || got.Object != "list" || len(got.Data) != 2 {
		t.Fatalf("%v %s", err, data)
	}
	if got.Data[0]["id"] != "gemma4:12b" || got.Data[0]["object"] != "model" || got.Data[1]["id"] != "plain" {
		t.Fatalf("the pulled models under their catalog names, and nothing else: %s", data)
	}
}

func TestProxyListsNothingWhenTheRouterHasNothingOfOurs(t *testing.T) {
	up := listUpstream(t, modelList("stranger"))
	_, front := proxyOver(t, up, catalogModels("plain")...)
	resp, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if string(data) != `{"data":[],"object":"list"}` {
		t.Fatalf("a list with nothing of ours is an empty list: %s", data)
	}
}

func TestProxyRefusesAModelListItCannotFilter(t *testing.T) {
	ours := PresetModel{Name: "gemma4:12b", ModelPath: "/m/g.gguf"}
	padded := `{"data":[{"id":"gemma4-12b","pad":"` + strings.Repeat("x", maxListAnswer) + `"}],"object":"list"}`
	cases := map[string]string{
		"not JSON":               `models`,
		"no data member":         `{"object":"list"}`,
		"data that is no list":   `{"data":"x","object":"list"}`,
		"an answer over the cap": padded,
	}
	for name, body := range cases {
		up := listUpstream(t, body)
		_, front := proxyOver(t, up, ours)
		resp, err := http.Get(front.URL + "/v1/models")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway || strings.Contains(string(data), "gemma4-12b") ||
			decodeProxyError(t, data).Error.Type != "server_error" {
			t.Errorf("%s: a list that cannot be filtered is not passed on: %d %.200s", name, resp.StatusCode, data)
		}
	}
}

func TestProxyRefusesAnEncodedModelList(t *testing.T) {
	up := newRecordingUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "br") // nothing the transport would decode
		_, _ = w.Write([]byte(modelList("gemma4-12b", "stranger")))
	})
	_, front := proxyOver(t, up, PresetModel{Name: "gemma4:12b", ModelPath: "/m/g.gguf"})
	resp, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("an encoded list cannot be filtered, so it is not passed on: %d", resp.StatusCode)
	}
}

func TestProxyFiltersAModelListTheRouterCompressed(t *testing.T) {
	up := newRecordingUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			_, _ = w.Write([]byte(modelList("gemma4-12b", "stranger")))
			return
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(modelList("gemma4-12b", "stranger")))
		_ = zw.Close()
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(buf.Bytes())
	})
	_, front := proxyOver(t, up, PresetModel{Name: "gemma4:12b", ModelPath: "/m/g.gguf"})
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/v1/models", nil)
	req.Header.Set("Accept-Encoding", "gzip") // set by hand, so the client leaves the answer as it comes
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" ||
		!strings.Contains(string(data), `"gemma4:12b"`) || strings.Contains(string(data), "stranger") {
		t.Fatalf("the proxy reads the list decoded and filters it: %d %v %s", resp.StatusCode, resp.Header, data)
	}
}

func hasCORS(h http.Header) bool {
	for key := range h {
		if strings.HasPrefix(key, "Access-Control-") {
			return true
		}
	}
	return false
}

func TestProxyDropsTheRoutersCORSHeaders(t *testing.T) {
	_, front := proxyHarness(t, 0)
	origin := http.Header{"Origin": {"https://evil.example"}}
	chat := postJSON(t, front.URL+"/v1/chat/completions", `{"model":"a","messages":[]}`, origin)
	_, _ = io.Copy(io.Discard, chat.Body)
	streamed := postJSON(t, front.URL+"/v1/chat/completions", `{"model":"a","stream":true,"messages":[]}`, origin)
	_, _ = io.Copy(io.Discard, streamed.Body)
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/v1/models", nil)
	req.Header.Set("Origin", "https://evil.example")
	listing, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = listing.Body.Close()
	for name, resp := range map[string]*http.Response{"chat": chat, "stream": streamed, "list": listing} {
		if resp.StatusCode != http.StatusOK || hasCORS(resp.Header) {
			t.Errorf("%s: the router approves every origin, so its CORS headers must not pass: %d %v",
				name, resp.StatusCode, resp.Header)
		}
	}
}

// corsHeaders are the Access-Control-* headers of an answer.
func corsHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for key := range h {
		if strings.HasPrefix(key, "Access-Control-") {
			out[key] = h.Get(key)
		}
	}
	return out
}

func TestProxyNeverForwardsAPreflightOrAnUnusedMethod(t *testing.T) {
	up := newRecordingUpstream(t, respondOK)
	b, bare := proxyOver(t, up, catalogModels("plain")...)
	// The daemon sets its own CORS headers for an origin it allows before the proxy runs; the proxy's
	// refusal adds none and leaves those alone.
	allowing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:5173")
		b.Proxy().ServeHTTP(w, r)
	}))
	t.Cleanup(allowing.Close)
	fronts := map[string]map[string]string{
		bare.URL:     {},
		allowing.URL: {"Access-Control-Allow-Origin": "http://localhost:5173"},
	}
	for _, method := range []string{http.MethodOptions, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		for front, want := range fronts {
			req, _ := http.NewRequest(method, front+"/v1/chat/completions", strings.NewReader(`{"model":"plain"}`))
			req.Header.Set("Origin", "http://localhost:5173")
			req.Header.Set("Access-Control-Request-Method", http.MethodPost)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD, POST" ||
				decodeProxyError(t, data).Error.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s: %d %v %s", method, resp.StatusCode, resp.Header, data)
			}
			if got := corsHeaders(resp.Header); !maps.Equal(got, want) {
				t.Errorf("%s: CORS headers %v, want only the daemon's own %v", method, got, want)
			}
		}
	}
	if n := len(up.requests()); n != 0 {
		t.Fatalf("no preflight and no other method may reach the router, upstream saw %d", n)
	}
}

func TestProxyErrorsCarryTheOpenAIShape(t *testing.T) {
	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, Attach: "http://127.0.0.1:1", APIKey: "x", State: testStore(dataDir), SleepIdleSeconds: 900,
		Logger: quietLogger(), Models: emptyCatalog})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	b.Proxy().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if body := decodeProxyError(t, rec.Body.Bytes()); rec.Code != http.StatusServiceUnavailable ||
		body.Error.Type != "unavailable_error" || body.Error.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
