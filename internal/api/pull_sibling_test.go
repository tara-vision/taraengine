package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
)

var (
	twoSHA     = strings.Repeat("d", 64)
	drafterSHA = strings.Repeat("e", 64)
)

// withTwoRepoEntry adds a catalog entry whose DFlash drafter lives in a repository of its own, and teaches the
// fake fetcher both files. The entry joins the harness's registry in place, so h.cat sees it too.
func withTwoRepoEntry(t *testing.T) func(*Deps) {
	t.Helper()
	return func(d *Deps) {
		body := `entries:
  - name: two:1b
    family: two
    hf_repo: org/two
    file: two.gguf
    sha256: ` + twoSHA + `
    size: 10
    tier: "16"
    dflash:
      hf_repo: org/drafter
      file: draft.gguf
      sha256: ` + drafterSHA + `
      size: 3
`
		extra, err := registry.Parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		d.Catalog.Registry.Entries = append(d.Catalog.Registry.Entries, extra.Entries...)
		f := d.Fetcher.(*fakeFetcher)
		f.artifacts["org/two/two.gguf"] = pull.Artifact{Repo: "org/two", File: "two.gguf", Size: 64, SHA256: twoSHA}
		f.artifacts["org/drafter/draft.gguf"] = pull.Artifact{Repo: "org/drafter", File: "draft.gguf", Size: 64, SHA256: drafterSHA}
	}
}

func TestPullFetchesASiblingFromItsOwnRepository(t *testing.T) {
	h := newHarness(t, "", withTwoRepoEntry(t))
	status, events := pullEvents(t, h, "two:1b")
	if status != http.StatusOK {
		t.Fatalf("pull: %d %v", status, events)
	}
	if !h.cat.Store.Exists("org/two", "two.gguf") || !h.cat.Store.Exists("org/drafter", "draft.gguf") {
		t.Fatalf("the files are not under their own repositories: %v", events)
	}
	if h.cat.Store.Exists("org/two", "draft.gguf") {
		t.Fatal("the drafter landed under the entry's repository")
	}
}

func TestDeleteRemovesASiblingUnderItsOwnRepository(t *testing.T) {
	h := newHarness(t, "", withTwoRepoEntry(t))
	pulled(t, h, "two:1b")
	if !h.cat.Store.Exists("org/drafter", "draft.gguf") {
		t.Fatal("the helper did not place the drafter under its own repository")
	}
	if resp, data := h.do(t, http.MethodDelete, "/api/models/two:1b", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %s", resp.StatusCode, data)
	}
	if h.cat.Store.Exists("org/two", "two.gguf") || h.cat.Store.Exists("org/drafter", "draft.gguf") {
		t.Fatal("a file survived the delete")
	}
	for _, dir := range []string{"org/two", "org/drafter"} {
		if _, err := os.Stat(filepath.Join(h.cat.Store.Dir, dir)); !os.IsNotExist(err) {
			t.Errorf("the emptied repository directory %s must be pruned: %v", dir, err)
		}
	}
}
