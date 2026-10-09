package pull

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const storiesSHA = "270cba1bd5109f42d03350f60406024560464db173c0e387d91f0426d3bd256d"

// stubRequest is one request the stub server received, with its body read out before it answered.
type stubRequest struct {
	Req  *http.Request
	Body string
}

// stubServer answers every request with status and body, and records each request it receives.
func stubServer(t *testing.T, status int, body []byte) (*httptest.Server, *[]stubRequest) {
	t.Helper()
	var seen []stubRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		seen = append(seen, stubRequest{Req: r.Clone(context.Background()), Body: string(sent)})
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// fixture reads the paths-info answer that the stub server sends back.
func fixture(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/paths-info.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return body
}

func TestResolveFindsTheLFSFile(t *testing.T) {
	srv, seen := stubServer(t, http.StatusOK, fixture(t))
	r := NewResolver("secret")
	r.BaseURL = srv.URL
	art, err := r.Resolve(context.Background(), "ggml-org/models-moved", "tinyllamas/stories260K.gguf")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if art.Size != 1185376 || art.SHA256 != storiesSHA {
		t.Fatalf("artifact wrong: %+v", art)
	}
	if len(*seen) != 1 {
		t.Fatalf("%d requests sent, want 1", len(*seen))
	}
	got := (*seen)[0]
	if got.Req.Method != http.MethodPost || got.Req.URL.Path != "/api/models/ggml-org/models-moved/paths-info/main" {
		t.Fatalf("request %s %s", got.Req.Method, got.Req.URL)
	}
	if got.Body != `{"paths":["tinyllamas/stories260K.gguf"]}` {
		t.Fatalf("body %q", got.Body)
	}
	if got.Req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content type %q", got.Req.Header.Get("Content-Type"))
	}
	if got.Req.Header.Get("Authorization") != "Bearer secret" {
		t.Fatalf("token not sent: %q", got.Req.Header.Get("Authorization"))
	}
}

func TestResolveSendsNoAuthorizationWithoutAToken(t *testing.T) {
	srv, seen := stubServer(t, http.StatusOK, fixture(t))
	r := NewResolver("")
	r.BaseURL = srv.URL
	if _, err := r.Resolve(context.Background(), "ggml-org/models-moved", "tinyllamas/stories260K.gguf"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("%d requests sent, want 1", len(*seen))
	}
	if sent := (*seen)[0].Req.Header.Values("Authorization"); len(sent) != 0 {
		t.Fatalf("authorization sent without a token: %q", sent)
	}
}

func TestResolveWorksForALiteralResolver(t *testing.T) {
	srv, _ := stubServer(t, http.StatusOK, fixture(t))
	r := &Resolver{BaseURL: srv.URL}
	art, err := r.Resolve(context.Background(), "ggml-org/models-moved", "tinyllamas/stories260K.gguf")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if art.SHA256 != storiesSHA {
		t.Fatalf("artifact wrong: %+v", art)
	}
}

func TestResolveErrors(t *testing.T) {
	body := fixture(t)
	cases := []struct {
		name   string
		status int
		body   []byte
		repo   string
		file   string
		want   string
	}{
		{"missing file", http.StatusOK, body, "ggml-org/models-moved", "nope.gguf", "has no file"},
		{"not lfs", http.StatusOK, body, "ggml-org/models-moved", "README.md", "not an LFS file"},
		{"lfs object with an empty oid", http.StatusOK, []byte(`[{"type": "file", "size": 4, "path": "a.gguf", "lfs": {"oid": "", "size": 4}}]`), "ggml-org/models-moved", "a.gguf", "not an LFS file"},
		{"directory named like the file", http.StatusOK, []byte(`[{"type": "directory", "oid": "0a1b", "path": "tinyllamas"}]`), "ggml-org/models-moved", "tinyllamas", "has no file"},
		{"repo missing", http.StatusNotFound, []byte(`{"error":"x"}`), "ggml-org/absent", "a.gguf", "answered 404"},
		{"bad json", http.StatusOK, []byte(`{`), "ggml-org/models-moved", "a.gguf", "decode response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := stubServer(t, tc.status, tc.body)
			r := NewResolver("")
			r.BaseURL = srv.URL
			_, err := r.Resolve(context.Background(), tc.repo, tc.file)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestResolveRefusesABadRepoWithoutARequest(t *testing.T) {
	srv, seen := stubServer(t, http.StatusOK, []byte(`[]`))
	r := NewResolver("")
	r.BaseURL = srv.URL
	for _, repo := range []string{"", "noslash", "a/b/c", "../x/y", "org/../y"} {
		if _, err := r.Resolve(context.Background(), repo, "a.gguf"); err == nil {
			t.Errorf("repo %q accepted", repo)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("%d requests were sent for invalid repositories", len(*seen))
	}
}

func TestResolveReportsRequestFailures(t *testing.T) {
	srv, _ := stubServer(t, http.StatusOK, fixture(t))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	t.Run("unparsable base URL", func(t *testing.T) {
		r := NewResolver("")
		r.BaseURL = "http://[::1"
		_, err := r.Resolve(context.Background(), "ggml-org/models-moved", "a.gguf")
		if err == nil {
			t.Fatal("an unparsable base URL resolved")
		}
		for _, want := range []string{"pull: resolve ggml-org/models-moved", "parse"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("want an error containing %q, got %v", want, err)
			}
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		r := NewResolver("")
		r.BaseURL = srv.URL
		_, err := r.Resolve(canceled, "ggml-org/models-moved", "tinyllamas/stories260K.gguf")
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("want the transport error, got %v", err)
		}
	})
}
