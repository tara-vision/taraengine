package llamaserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend"
)

const modelsBody = `{"data":[
 {"id":"broken","object":"model","owned_by":"llamacpp","status":{"value":"unloaded","exit_code":1,"failed":true,"args":[],"preset":"[broken]\nmodel = /m/broken.gguf\n"},"architecture":{"input_modalities":["text"],"output_modalities":["text"]},"source":"preset","can_remove":false},
 {"id":"stories","object":"model","owned_by":"llamacpp","status":{"value":"loaded","args":["llama-server","--ctx-size","512"],"preset":"[stories]\nctx-size = 512\nmodel = /m/s.gguf\n"},"architecture":{"input_modalities":["text"],"output_modalities":["text"]},"source":"preset","can_remove":false}
],"object":"list"}`

const propsBody = `{"default_generation_settings":{"id":0,"n_ctx":512,"params":{}},"total_slots":4,"model_path":"/m/s.gguf","model_alias":"stories",
 "chat_template":"{%- for message in messages -%}{% if enable_thinking %}<think>{% endif %}{%- endfor -%}",
 "chat_template_caps":{"supports_tools":true,"supports_tool_calls":true,"supports_reasoning_effort":false,"supports_system_role":true},
 "modalities":{"vision":true,"video":false,"audio":false},"is_sleeping":false,"build_info":"b11514-de7fa0a3c"}`

func routerStub(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Invalid API Key","type":"authentication_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "GET /models":
			_, _ = w.Write([]byte(modelsBody))
		case "POST /models/load":
			if strings.Contains(readBody(r), `"nope"`) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"message":"File Not Found","type":"not_found_error","code":404}}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true}`))
		case "POST /models/unload":
			if strings.Contains(readBody(r), `"idle"`) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":400,"message":"model is not running","type":"invalid_request_error"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true}`))
		case "GET /props":
			if r.URL.Query().Get("model") != "stories" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":400,"message":"model 'x' not found","type":"invalid_request_error"}}`))
				return
			}
			_, _ = w.Write([]byte(propsBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func readBody(r *http.Request) string {
	b := make([]byte, 1024)
	n, _ := r.Body.Read(b)
	return string(b[:n])
}

//nolint:gocyclo // one ordered scenario over the captured router shapes: each call reads what the one before it left
func TestClientParsesTheRouterShapes(t *testing.T) {
	srv, calls := routerStub(t)
	c := NewClient(srv.URL, "key")
	ctx := context.Background()
	if err := c.Health(ctx); err != nil {
		t.Fatalf("health: %v", err)
	}
	models, err := c.Models(ctx, true)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "broken" || !models[0].Failed || models[0].ExitCode != 1 || models[0].Status != "unloaded" {
		t.Fatalf("broken model parsed wrong: %+v", models[0])
	}
	if models[1].Status != "loaded" || !strings.Contains(models[1].Preset, "ctx-size = 512") {
		t.Fatalf("stories parsed wrong: %+v", models[1])
	}
	if (*calls)[1] != "GET /models?reload=1" {
		t.Fatalf("reload query missing: %s", (*calls)[1])
	}
	if err := c.Load(ctx, "stories"); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := c.Load(ctx, "nope"); !errors.Is(err, backend.ErrUnknownModel) {
		t.Fatalf("unknown model on load: %v", err)
	}
	if err := c.Unload(ctx, "stories"); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if err := c.Unload(ctx, "idle"); err != nil {
		t.Fatalf("unloading an idle model must be a no-op: %v", err)
	}
	p, err := c.Props(ctx, "stories")
	if err != nil {
		t.Fatalf("props: %v", err)
	}
	if p.ContextLength != 512 || p.ModelPath != "/m/s.gguf" || !p.SupportsTools || !p.Vision || !p.Thinking {
		t.Fatalf("props parsed wrong: %+v", p)
	}
	var rerr *RouterError
	if _, err := c.Props(ctx, "x"); !errors.As(err, &rerr) || rerr.Code != 400 {
		t.Fatalf("props error: %v", err)
	}
}

func TestClientReportsAuthAndTransportErrors(t *testing.T) {
	srv, _ := routerStub(t)
	c := NewClient(srv.URL, "wrong")
	var rerr *RouterError
	if err := c.Health(context.Background()); !errors.As(err, &rerr) || rerr.Code != 401 {
		t.Fatalf("want a 401 router error, got %v", err)
	}
	down := NewClient("http://127.0.0.1:1", "key")
	if err := down.Health(context.Background()); err == nil {
		t.Fatal("a closed port must fail")
	}
}

func serveBody(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClientModelsWithoutReloadUsesPlainPath(t *testing.T) {
	srv, calls := routerStub(t)
	models, err := NewClient(srv.URL, "key").Models(context.Background(), false)
	if err != nil || len(models) != 2 {
		t.Fatalf("models without reload: %v (%d models)", err, len(models))
	}
	if (*calls)[0] != "GET /models" {
		t.Fatalf("plain models call: %s", (*calls)[0])
	}
	var rerr *RouterError
	if _, err := NewClient(srv.URL, "wrong").Models(context.Background(), false); !errors.As(err, &rerr) || rerr.Code != 401 {
		t.Fatalf("models with a wrong key: %v", err)
	}
}

func TestClientLoadAndUnloadReturnOtherErrors(t *testing.T) {
	srv, _ := routerStub(t)
	ctx := context.Background()
	wrong := NewClient(srv.URL, "wrong")
	var rerr *RouterError
	if err := wrong.Load(ctx, "stories"); !errors.As(err, &rerr) || rerr.Code != 401 || errors.Is(err, backend.ErrUnknownModel) {
		t.Fatalf("load with a wrong key: %v", err)
	}
	if err := wrong.Unload(ctx, "stories"); !errors.As(err, &rerr) || rerr.Code != 401 {
		t.Fatalf("unload with a wrong key: %v", err)
	}
	busy := serveBody(t, http.StatusBadRequest, `{"error":{"code":400,"message":"model is busy"}}`)
	if err := NewClient(busy.URL, "key").Unload(ctx, "stories"); !errors.As(err, &rerr) || rerr.Message != "model is busy" {
		t.Fatalf("unload of a busy model must fail: %v", err)
	}
}

func TestClientRejectsMalformedBaseURL(t *testing.T) {
	bad := NewClient("http://[::1", "key")
	if err := bad.Health(context.Background()); err == nil {
		t.Fatal("a malformed base URL must fail")
	}
}

func TestClientRejectsUnencodableBody(t *testing.T) {
	srv, calls := routerStub(t)
	c := NewClient(srv.URL, "key")
	if err := c.call(context.Background(), http.MethodPost, "/models/load", make(chan int), nil); err == nil {
		t.Fatal("a body that cannot be encoded must fail")
	}
	if len(*calls) != 0 {
		t.Fatalf("request sent anyway: %v", *calls)
	}
}

func TestClientRejectsTruncatedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(`{"data":`))
	}))
	t.Cleanup(srv.Close)
	if _, err := NewClient(srv.URL, "key").Models(context.Background(), false); err == nil {
		t.Fatal("a body cut short must fail")
	}
}

func TestClientReportsUndecodableBody(t *testing.T) {
	srv := serveBody(t, http.StatusOK, `{"data":"not a list"}`)
	if _, err := NewClient(srv.URL, "key").Models(context.Background(), false); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("want a decode error, got %v", err)
	}
}

func TestClientRouterErrorFallsBackToStatus(t *testing.T) {
	ctx := context.Background()
	var rerr *RouterError
	plain := serveBody(t, http.StatusServiceUnavailable, "upstream down")
	if err := NewClient(plain.URL, "key").Health(ctx); !errors.As(err, &rerr) || rerr.Code != 503 || rerr.Message != "Service Unavailable" {
		t.Fatalf("plain-text error: %v", err)
	}
	if got := rerr.Error(); got != "router answered 503: Service Unavailable" {
		t.Fatalf("error text %q", got)
	}
	empty := serveBody(t, http.StatusInternalServerError, `{"error":{}}`)
	if err := NewClient(empty.URL, "key").Health(ctx); !errors.As(err, &rerr) || rerr.Code != 500 || rerr.Message != "Internal Server Error" {
		t.Fatalf("empty error body: %v", err)
	}
}

func TestClientPropsReadsThinkingMarkers(t *testing.T) {
	cases := []struct {
		name     string
		template string
		want     bool
	}{
		{name: "enable_thinking marker", template: "{% if enable_thinking %}", want: true},
		{name: "think tag marker", template: "<think>", want: true},
		{name: "no marker", template: "plain chat template", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serveBody(t, http.StatusOK, `{"default_generation_settings":{"n_ctx":8},"chat_template":"`+tc.template+`"}`)
			p, err := NewClient(srv.URL, "key").Props(context.Background(), "stories")
			if err != nil {
				t.Fatalf("props: %v", err)
			}
			if p.Thinking != tc.want {
				t.Fatalf("thinking = %t, want %t", p.Thinking, tc.want)
			}
		})
	}
}

func TestClientPropsEscapesModelID(t *testing.T) {
	srv, calls := routerStub(t)
	_, err := NewClient(srv.URL, "key").Props(context.Background(), "a&b")
	var rerr *RouterError
	if !errors.As(err, &rerr) || rerr.Code != 400 {
		t.Fatalf("props for an unknown id: %v", err)
	}
	if (*calls)[0] != "GET /props?model=a%26b" {
		t.Fatalf("query not escaped: %s", (*calls)[0])
	}
}

func TestClientReportsARouterNobodyListensForAsNotRunning(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close() // its port now refuses connections
	c := NewClient(gone.URL, "key")
	err := c.Health(context.Background())
	if !errors.Is(err, backend.ErrNotRunning) {
		t.Fatalf("a refused connection must read as a backend that is not running: %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("the child's address must stay out of the error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Health(ctx); errors.Is(err, backend.ErrNotRunning) || !errors.Is(err, context.Canceled) {
		t.Fatalf("a caller that gave up gets its own error back: %v", err)
	}
}
