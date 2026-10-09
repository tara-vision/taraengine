package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tara-vision/taraengine/internal/pull"
)

// hubServer plays the two Hugging Face endpoints the fetcher uses and records every Authorization header.
type hubServer struct {
	*httptest.Server
	mu   sync.Mutex
	auth []string
}

func (s *hubServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auth...)
}

func newHub(t *testing.T, repo, file string, body []byte) *hubServer {
	t.Helper()
	sum := sha256.Sum256(body)
	hub := &hubServer{}
	hub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.mu.Lock()
		hub.auth = append(hub.auth, r.Header.Get("Authorization"))
		hub.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/models/"+repo+"/paths-info/main":
			entry := map[string]any{"type": "file", "path": file, "size": len(body), "lfs": map[string]string{"oid": hex.EncodeToString(sum[:])}}
			_ = json.NewEncoder(w).Encode([]any{entry})
		case r.Method == http.MethodGet && r.URL.Path == "/"+repo+"/resolve/main/"+file:
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	return hub
}

func TestHFFetcherResolvesAndDownloads(t *testing.T) {
	body := []byte("0123456789abcdef")
	hub := newHub(t, "org/repo", "m.gguf", body)
	f := newHFFetcher(hub.URL, "tok")
	ctx := context.Background()

	art, err := f.Resolve(ctx, "org/repo", "m.gguf")
	sum := sha256.Sum256(body)
	if err != nil || art.Repo != "org/repo" || art.File != "m.gguf" || art.Size != int64(len(body)) || art.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("resolve: %+v %v", art, err)
	}

	dest := filepath.Join(t.TempDir(), "org", "repo", "m.gguf")
	var stages []string
	if err := f.Fetch(ctx, art, dest, func(p pull.Progress) { stages = append(stages, p.Stage) }); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != string(body) {
		t.Fatalf("blob: %q %v", got, err)
	}
	if strings.Join(stages, ",") != "downloading,verifying,done" {
		t.Fatalf("progress must reach the caller stage by stage: %v", stages)
	}
	for _, header := range hub.seen() {
		if header != "Bearer tok" {
			t.Errorf("a request went out with Authorization %q", header)
		}
	}
	if len(hub.seen()) != 2 {
		t.Fatalf("one resolve and one download: %v", hub.seen())
	}
}

func TestHFFetcherReportsWhatHuggingFaceRefuses(t *testing.T) {
	hub := newHub(t, "org/repo", "m.gguf", []byte("x"))
	f := newHFFetcher(hub.URL, "")
	ctx := context.Background()
	if _, err := f.Resolve(ctx, "org/other", "m.gguf"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("resolve of an unknown repository: %v", err)
	}
	art := pull.Artifact{Repo: "org/other", File: "m.gguf", Size: 1, SHA256: "abc"}
	dest := filepath.Join(t.TempDir(), "m.gguf")
	if err := f.Fetch(ctx, art, dest, func(pull.Progress) {}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("fetch of an unknown repository: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("a failed download left a blob: %v", err)
	}
	for _, header := range hub.seen() {
		if header != "" {
			t.Errorf("without a token no Authorization header may be sent: %q", header)
		}
	}
}

func TestNewHFFetcherTalksToHuggingFaceOnly(t *testing.T) {
	f, ok := NewHFFetcher("tok").(*hfFetcher)
	if !ok {
		t.Fatalf("NewHFFetcher returned %T", NewHFFetcher("tok"))
	}
	if f.baseURL != "https://huggingface.co" || f.resolver.BaseURL != "https://huggingface.co" || f.token != "tok" {
		t.Fatalf("fetcher: %+v resolver %+v", f, f.resolver)
	}
}
