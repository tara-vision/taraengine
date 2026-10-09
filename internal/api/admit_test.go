package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/catalog"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/pkg/api"
)

// addFile makes a Hugging Face file available to the fake fetcher.
func addFile(h *harness, repo, file string) {
	h.fetcher.artifacts[repo+"/"+file] = pull.Artifact{Repo: repo, File: file, Size: 16, SHA256: "abc"}
}

// refusedPull pulls ref in both modes and checks that the daemon turns it away with status and a message that
// holds every part, and that it changed nothing.
func refusedPull(t *testing.T, h *harness, ref string, status int, parts ...string) {
	t.Helper()
	before, _ := h.cat.Index.Load()
	refreshes := h.fake.Refreshes
	for _, path := range []string{"/api/pull", "/api/pull?wait=1"} {
		resp, data := h.do(t, http.MethodPost, path, "", api.PullRequest{Model: ref})
		body := decode[api.Error](t, data).Error
		if resp.StatusCode != status || body.Hint == "" || resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s as %q: %d %s", path, ref, resp.StatusCode, data)
		}
		for _, part := range parts {
			if !strings.Contains(body.Message, part) {
				t.Errorf("%s as %q: the message %q lacks %q", path, ref, body.Message, part)
			}
		}
	}
	after, _ := h.cat.Index.Load()
	if len(after) != len(before) || h.fake.Refreshes != refreshes {
		t.Errorf("%q: a refused pull must change nothing: %+v %d", ref, after, h.fake.Refreshes)
	}
}

func TestPullRefusesAFileWhoseNameSharesARouterIDWithACatalogModel(t *testing.T) {
	h := newHarness(t, "")
	dir := t.TempDir()
	clash := filepath.Join(dir, "gemma4-12b.gguf") // gemma4:12b is served as gemma4-12b
	if err := os.WriteFile(clash, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	same := filepath.Join(dir, "glm-4.7:flash.gguf") // glm-4.7-flash is the catalog name itself
	if err := os.WriteFile(same, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	addFile(h, "org/repo", "gemma4-12b.gguf")
	for ref, other := range map[string]string{
		"hf://org/repo/gemma4-12b.gguf": "gemma4:12b", clash: "gemma4:12b", same: "glm-4.7-flash",
	} {
		refusedPull(t, h, ref, http.StatusConflict, "engine id", other)
	}
	if h.cat.Store.Exists("org/repo", "gemma4-12b.gguf") {
		t.Fatal("nothing is downloaded for a name that is refused")
	}
	if _, err := os.Lstat(h.cat.Store.Path("local", "gemma4-12b.gguf")); !os.IsNotExist(err) {
		t.Fatalf("nothing is linked for a name that is refused: %v", err)
	}
}

func TestPullRefusesANameThatSharesARouterIDWithAnotherIndexedModel(t *testing.T) {
	h := newHarness(t, "")
	addHFModel(t, h, "x-y", "org/repo", "x-y.gguf")
	addFile(h, "org/repo", "x:y.gguf")
	refusedPull(t, h, "hf://org/repo/x:y.gguf", http.StatusConflict, "x:y", "x-y", "engine id")
	other := t.TempDir()
	local := filepath.Join(other, "x:y.gguf")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	refusedPull(t, h, local, http.StatusConflict, "x:y", "x-y")

	h = newHarness(t, "")
	addHFModel(t, h, "p:q", "org/repo", "p:q.gguf")
	addFile(h, "org/repo", "p-q.gguf")
	refusedPull(t, h, "hf://org/repo/p-q.gguf", http.StatusConflict, "p-q", "p:q")
}

func TestPullRefusesACatalogModelWhoseRouterIDAnIndexedNameHolds(t *testing.T) {
	h := newHarness(t, "")
	addHFModel(t, h, "gemma4-12b", "org/repo", "gemma4-12b.gguf") // an index that an older daemon let in
	refusedPull(t, h, "gemma4:12b", http.StatusConflict, "gemma4:12b", "gemma4-12b", "engine id")
}

func TestPullOfAModelWithTheSameNameIsNoClash(t *testing.T) {
	h := newHarness(t, "")
	for range 2 {
		if resp, data := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "hf://org/repo/extra.gguf"}); resp.StatusCode != 200 {
			t.Fatalf("the same file again is a pull that resumes: %d %s", resp.StatusCode, data)
		}
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "glm-4.7-flash"}); resp.StatusCode != 200 {
		t.Fatalf("a catalog pull next to an indexed model with another id: %d", resp.StatusCode)
	}
}

func TestPullRefusesADifferentFileUnderAnIndexedName(t *testing.T) {
	h := newHarness(t, "")
	addFile(h, "other/repo", "extra.gguf")
	addFile(h, "org/repo", "sub/extra.gguf")
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "hf://org/repo/extra.gguf"}); resp.StatusCode != 200 {
		t.Fatalf("first pull: %d", resp.StatusCode)
	}
	first := h.cat.Store.Path("org/repo", "extra.gguf")
	for _, ref := range []string{"hf://other/repo/extra.gguf", "hf://org/repo/sub/extra.gguf"} {
		refusedPull(t, h, ref, http.StatusConflict, "extra", "hf://org/repo/extra.gguf")
	}
	local := filepath.Join(t.TempDir(), "extra.gguf")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	refusedPull(t, h, local, http.StatusConflict, "extra", "hf://org/repo/extra.gguf")
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("the first file stays where it is, owned by its name: %v", err)
	}
	if entries, _ := h.cat.Index.Load(); len(entries) != 1 || entries[0].Repo != "org/repo" {
		t.Fatalf("the index still says what the first pull said: %+v", entries)
	}
}

func TestPullRefusesAFileOfAnotherPathUnderALocalName(t *testing.T) {
	h := newHarness(t, "")
	one, two := filepath.Join(t.TempDir(), "Mine.gguf"), filepath.Join(t.TempDir(), "Mine.gguf")
	for _, p := range []string{one, two} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: one}); resp.StatusCode != 200 {
		t.Fatalf("first pull: %d", resp.StatusCode)
	}
	refusedPull(t, h, two, http.StatusConflict, "Mine", "a local file")
	addFile(h, "org/repo", "Mine.gguf")
	refusedPull(t, h, "hf://org/repo/Mine.gguf", http.StatusConflict, "Mine", "a local file")
	if resp, data := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: one}); resp.StatusCode != 200 {
		t.Fatalf("the same path again links the same file: %d %s", resp.StatusCode, data)
	}
	if target, err := os.Readlink(h.cat.Store.Path("local", "Mine.gguf")); err != nil || target != one {
		t.Fatalf("the link: %q %v", target, err)
	}
}

func TestPullAcceptsALocalNameWhoseLinkIsGone(t *testing.T) {
	h := newHarness(t, "")
	one, two := filepath.Join(t.TempDir(), "Mine.gguf"), filepath.Join(t.TempDir(), "Mine.gguf")
	for _, p := range []string{one, two} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.cat.Index.Add(catalog.IndexEntry{Name: "Mine", Path: h.cat.Store.Path("local", "Mine.gguf"), Size: 1}); err != nil {
		t.Fatal(err)
	}
	if resp, data := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: two}); resp.StatusCode != 200 {
		t.Fatalf("an entry with no link holds no file: %d %s", resp.StatusCode, data)
	}
}

func TestPullRefusesANameThatSharesARouterIDWithAPullInFlight(t *testing.T) {
	h := newHarness(t, "")
	addFile(h, "org/repo", "x-y.gguf")
	addFile(h, "org/repo", "x:y.gguf")
	holdPulls(t, h, "x-y.gguf")
	first := make(chan int, 1)
	go func() {
		status, _, _, err := h.request(http.MethodPost, "/api/pull?wait=1", "application/json", "", strings.NewReader(`{"model":"hf://org/repo/x-y.gguf"}`))
		if err != nil {
			status = -1
		}
		first <- status
	}()
	waitFor(t, h.fetcher.started)
	refusedPull(t, h, "hf://org/repo/x:y.gguf", http.StatusConflict, "x:y", "x-y", "engine id", "running")
	close(h.fetcher.gate)
	if status := waitFor(t, first); status != 200 {
		t.Fatalf("the pull that held the name: %d", status)
	}
}

func TestADeleteIsNoClashWithAPullOfTheSameRouterIDInEitherOrder(t *testing.T) {
	s := newServer(newHarness(t, "").deps)
	for _, order := range [][2]string{{opDelete, opPull}, {opPull, opDelete}} {
		first, refused := s.claim("x-y", order[0])
		if refused != nil {
			t.Fatal(refused.message)
		}
		second, refused := s.claim("x:y", order[1])
		if refused != nil {
			t.Fatalf("a %s of x-y and a %s of x:y: a delete creates no clash: %v", order[0], order[1], refused.message)
		}
		second()
		first()
	}
	first, _ := s.claim("x-y", opPull)
	defer first()
	if _, refused := s.claim("x:y", opPull); refused == nil || refused.status != http.StatusConflict {
		t.Fatalf("two pulls of one router id are a clash: %+v", refused)
	}
}

func TestPullFailsAtTheDoorWhenTheIndexCannotBeRead(t *testing.T) {
	h := newHarness(t, "")
	breakIndex(t, h)
	local := filepath.Join(t.TempDir(), "Mine.gguf")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"hf://org/repo/extra.gguf", local, "glm-4.7-flash"} {
		for _, path := range []string{"/api/pull", "/api/pull?wait=1"} {
			resp, data := h.do(t, http.MethodPost, path, "", api.PullRequest{Model: ref})
			if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(data), "parse index") {
				t.Errorf("%s as %q: %d %s", path, ref, resp.StatusCode, data)
			}
		}
	}
	if logs := h.logs.String(); !strings.Contains(logs, "catalog unreadable") {
		t.Errorf("a broken index is an error for the operator, so it is logged: %s", logs)
	}
	if h.cat.Store.Exists("org/repo", "extra.gguf") || h.fake.Refreshes != 0 {
		t.Fatal("nothing is downloaded when the index cannot be read")
	}
	if err := os.Remove(h.cat.Index.Path); err != nil {
		t.Fatal(err)
	}
	if resp, data := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "hf://org/repo/extra.gguf"}); resp.StatusCode != 200 {
		t.Fatalf("the refusals must not leave the name claimed: %d %s", resp.StatusCode, data)
	}
}
